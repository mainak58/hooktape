package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

	// The result is recorded, so the table shows it without a reload.
	stored, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ForwardStatus != 202 {
		t.Errorf("forward_status = %d", stored.ForwardStatus)
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
