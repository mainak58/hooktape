package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mainak58/hooktape/internal/forward"
	"github.com/mainak58/hooktape/internal/model"
	"github.com/mainak58/hooktape/internal/store"
)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	h := &Handler{Store: s, Client: forward.NewClient(2 * time.Second)}
	h.SetStatus(Status{Version: "test", Catcher: "http://127.0.0.1:8080", EndpointHint: "stripe"})

	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)

	return srv, s
}

func insert(t *testing.T, s *store.Store, body string) int64 {
	t.Helper()

	id, err := s.Insert(context.Background(), &model.Request{
		Endpoint:  "stripe",
		Method:    "POST",
		Path:      "/",
		Headers:   map[string][]string{"Content-Type": {"application/json"}},
		Body:      []byte(body),
		CreatedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	return id
}

func call(t *testing.T, method, url string, body any, dest any) int {
	t.Helper()

	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, url, payload)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if dest != nil && res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(dest); err != nil {
			t.Fatalf("decode %s %s: %v", method, url, err)
		}
	}

	return res.StatusCode
}

func TestPageAndStatus(t *testing.T) {
	srv, _ := newTestServer(t)

	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	html, _ := io.ReadAll(res.Body)
	if !bytes.Contains(html, []byte("hooktape")) {
		t.Errorf("index page does not look like the dashboard: %.80s", html)
	}

	var status Status
	if code := call(t, "GET", srv.URL+"/api/status", nil, &status); code != 200 {
		t.Fatalf("status = %d", code)
	}
	if status.Version != "test" || status.EndpointHint != "stripe" {
		t.Errorf("status = %+v", status)
	}
}

func TestListAndGetRequest(t *testing.T) {
	srv, s := newTestServer(t)
	id := insert(t, s, `{"hello":"world"}`)

	var list []model.Request
	if code := call(t, "GET", srv.URL+"/api/requests?limit=10", nil, &list); code != 200 {
		t.Fatalf("list = %d", code)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("list = %+v", list)
	}
	// The list view is for the table only, so it must not carry bodies.
	if list[0].Body != nil {
		t.Errorf("list returned a body: %q", list[0].Body)
	}
	if list[0].BodySize != len(`{"hello":"world"}`) {
		t.Errorf("body_size = %d", list[0].BodySize)
	}

	var one model.Request
	if code := call(t, "GET", srv.URL+"/api/requests/1", nil, &one); code != 200 {
		t.Fatalf("get = %d", code)
	}
	if string(one.Body) != `{"hello":"world"}` {
		t.Errorf("body = %q", one.Body)
	}

	if code := call(t, "GET", srv.URL+"/api/requests/999", nil, nil); code != 404 {
		t.Errorf("missing id = %d, want 404", code)
	}
	if code := call(t, "GET", srv.URL+"/api/requests/abc", nil, nil); code != 400 {
		t.Errorf("bad id = %d, want 400", code)
	}
}

func TestReplay(t *testing.T) {
	srv, s := newTestServer(t)
	id := insert(t, s, `{"hello":"world"}`)
	if err := s.SetForwardResult(context.Background(), id, 200, 41, ""); err != nil {
		t.Fatal(err)
	}

	var got []byte
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		if r.Header.Get("X-Hooktape-Replay") != "true" {
			t.Errorf("missing replay header")
		}
		w.WriteHeader(202)
		w.Write([]byte("thanks"))
	}))
	defer app.Close()

	var res struct {
		Status int    `json:"status"`
		Error  string `json:"error"`
		Body   string `json:"body"`
	}
	code := call(t, "POST", srv.URL+"/api/requests/1/replay", map[string]string{"target": app.URL}, &res)
	if code != 200 || res.Status != 202 || res.Body != "thanks" || res.Error != "" {
		t.Fatalf("replay = %d %+v", code, res)
	}
	if string(got) != `{"hello":"world"}` {
		t.Errorf("app saw body %q", got)
	}

	// The replay is recorded beside the live delivery, never over it.
	stored, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ReplayStatus != 202 || stored.ForwardStatus != 200 || stored.ForwardMs != 41 {
		t.Errorf("replay_status = %d, forward = %d %dms", stored.ReplayStatus, stored.ForwardStatus, stored.ForwardMs)
	}

	for _, target := range []any{map[string]string{"target": ""}, map[string]string{"target": "file:///etc/passwd"}, map[string]string{"target": "localhost:3000"}} {
		if code := call(t, "POST", srv.URL+"/api/requests/1/replay", target, nil); code != 400 {
			t.Errorf("replay to %v = %d, want 400", target, code)
		}
	}
}

func TestDelete(t *testing.T) {
	srv, s := newTestServer(t)
	for range 3 {
		insert(t, s, "x")
	}

	var res struct{ Deleted int64 }
	if code := call(t, "POST", srv.URL+"/api/requests/delete", map[string]any{"ids": []int64{1, 2}}, &res); code != 200 || res.Deleted != 2 {
		t.Fatalf("delete ids = %d %+v", code, res)
	}

	res.Deleted = 0
	if code := call(t, "POST", srv.URL+"/api/requests/delete", map[string]any{"all": true}, &res); code != 200 || res.Deleted != 1 {
		t.Fatalf("delete all = %d %+v", code, res)
	}

	left, err := s.List(context.Background(), store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d requests left", len(left))
	}

	if code := call(t, "POST", srv.URL+"/api/requests/delete", map[string]any{}, nil); code != 400 {
		t.Errorf("empty delete = %d, want 400", code)
	}
}

func post(t *testing.T, url, contentType, body string, headers map[string]string) int {
	t.Helper()

	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	return res.StatusCode
}

func TestCrossSiteWritesAreRefused(t *testing.T) {
	srv, s := newTestServer(t)
	insert(t, s, "x")
	url := srv.URL + "/api/requests/delete"
	all := `{"all":true}`

	for _, tt := range []struct {
		name        string
		contentType string
		headers     map[string]string
		want        int
	}{
		{"cross-site fetch", "application/json", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"same-site fetch", "application/json", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"foreign Origin", "application/json", map[string]string{"Origin": "https://evil.example"}, 403},
		{"simple request", "text/plain;charset=UTF-8", nil, 415},
		{"form post", "application/x-www-form-urlencoded", nil, 415},
		{"no content type", "", nil, 415},
	} {
		if code := post(t, url, tt.contentType, all, tt.headers); code != tt.want {
			t.Errorf("%s = %d, want %d", tt.name, code, tt.want)
		}
	}

	left, err := s.List(context.Background(), store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Fatalf("a refused request deleted rows: %d left", len(left))
	}

	// The page's own request has to keep working.
	code := post(t, url, "application/json; charset=utf-8", all, map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": srv.URL})
	if code != 200 {
		t.Errorf("same-origin delete = %d, want 200", code)
	}
}

func TestHostCheck(t *testing.T) {
	srv, s := newTestServer(t)
	insert(t, s, "secret")

	for host, want := range map[string]int{
		"evil.example":      403,
		"evil.example:8081": 403,
		"127.0.0.1.nip.io":  403,
		"127.0.0.1:8081":    200,
		"localhost:8081":    200,
		"LOCALHOST":         200,
		"[::1]:8081":        200,
		"[::1]":             200,
		"192.168.0.10:8081": 200,
	} {
		for _, path := range []string{"/", "/api/requests"} {
			req, err := http.NewRequest("GET", srv.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = host

			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != want {
				t.Errorf("Host %s %s = %d, want %d", host, path, res.StatusCode, want)
			}
		}
	}
}

func TestDeleteTakesOneSelector(t *testing.T) {
	srv, s := newTestServer(t)
	insert(t, s, "x")
	url := srv.URL + "/api/requests/delete"

	for _, body := range []map[string]any{
		{"ids": []int64{1}, "older_than_ms": 1},
		{"ids": []int64{1}, "all": true},
		{"older_than_ms": 1, "all": true},
	} {
		if code := call(t, "POST", url, body, nil); code != 400 {
			t.Errorf("delete %v = %d, want 400", body, code)
		}
	}

	left, err := s.List(context.Background(), store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("%d requests left, want 1", len(left))
	}

	var res struct{ Deleted int64 }
	cutoff := time.Now().Add(time.Hour).UnixMilli()
	if code := call(t, "POST", url, map[string]any{"older_than_ms": cutoff}, &res); code != 200 || res.Deleted != 1 {
		t.Errorf("delete older = %d %+v", code, res)
	}
}

func TestReplayResultNotSaved(t *testing.T) {
	srv, s := newTestServer(t)
	id := insert(t, s, "x")

	// The row disappears mid-replay, as when another tab deletes it.
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.DeleteByIDs(context.Background(), []int64{id}); err != nil {
			t.Error(err)
		}
	}))
	defer app.Close()

	var res struct {
		Status      int    `json:"status"`
		RecordError string `json:"record_error"`
	}
	code := call(t, "POST", srv.URL+"/api/requests/1/replay", map[string]string{"target": app.URL}, &res)
	if code != 200 || res.Status != 200 || res.RecordError == "" {
		t.Errorf("replay = %d %+v", code, res)
	}
}

func TestReplayRefusesTruncatedBody(t *testing.T) {
	srv, s := newTestServer(t)
	id, err := s.Insert(context.Background(), &model.Request{
		Endpoint: "stripe", Method: "POST", Path: "/", Headers: map[string][]string{},
		Body: []byte("cut"), Truncated: true, CreatedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a truncated body reached the app")
	}))
	defer app.Close()

	var res struct {
		Status int    `json:"status"`
		Error  string `json:"error"`
	}
	code := call(t, "POST", srv.URL+"/api/requests/1/replay", map[string]string{"target": app.URL}, &res)
	if code != 200 || res.Status != 0 || res.Error != forward.ErrTruncated.Error() {
		t.Fatalf("replay = %d %+v", code, res)
	}

	var list []model.Request
	if code := call(t, "GET", srv.URL+"/api/requests", nil, &list); code != 200 || len(list) != 1 {
		t.Fatalf("list = %d %+v", code, list)
	}
	if !list[0].Truncated || list[0].ID != id || list[0].ReplayError != forward.ErrTruncated.Error() {
		t.Errorf("listed row = %+v", list[0])
	}
}

func TestReplayKeepPath(t *testing.T) {
	for _, keep := range []bool{false, true} {
		s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.Insert(context.Background(), &model.Request{
			Endpoint: "stripe", Method: "POST", Path: "/admin/users", Headers: map[string][]string{}, CreatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}

		var got string
		app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.URL.Path }))
		defer app.Close()

		h := &Handler{Store: s, Client: forward.NewClient(2 * time.Second), KeepPath: keep}
		srv := httptest.NewServer(h.Routes())
		defer srv.Close()

		if code := call(t, "POST", srv.URL+"/api/requests/1/replay", map[string]string{"target": app.URL + "/hook"}, nil); code != 200 {
			t.Fatalf("replay = %d", code)
		}

		want := "/hook"
		if keep {
			want = "/hook/admin/users"
		}
		if got != want {
			t.Errorf("KeepPath %v reached %q, want %q", keep, got, want)
		}
	}
}

func TestFailHidesStoreDetail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-name.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	var logged string
	h := &Handler{Store: s, Logf: func(format string, args ...any) { logged = fmt.Sprintf(format, args...) }}
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	s.Close() // every query now fails with a raw database error

	res, err := http.Get(srv.URL + "/api/requests")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != 500 || strings.Contains(string(body), "closed") {
		t.Errorf("response = %d %q", res.StatusCode, body)
	}
	if logged == "" {
		t.Error("the detail was not logged")
	}
}
