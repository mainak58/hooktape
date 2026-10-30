package catcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mainak58/hooktape/internal/model"
	"github.com/mainak58/hooktape/internal/store"
)

type fakeForwarder chan *model.Request

func (f fakeForwarder) Forward(r *model.Request) { f <- r }

// newTestServer fills in h.Store and serves h.Routes() until the test ends.
func newTestServer(t *testing.T, h *Handler) (*httptest.Server, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	h.Store = s
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, s
}

func send(t *testing.T, method, url string, body []byte, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	reply, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(reply)
}

func storedCount(t *testing.T, s *store.Store) int {
	t.Helper()
	got, err := s.List(context.Background(), store.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return len(got)
}

func TestCaptureStoresRequest(t *testing.T) {
	srv, s := newTestServer(t, &Handler{})
	body := []byte(`{"type":"charge.succeeded"}`)

	status, reply := send(t, "POST", srv.URL+"/h/demo/stripe?x=1", body, nil)
	if status != http.StatusOK || reply != `{"ok":true,"id":1}` {
		t.Fatalf("got %d %q", status, reply)
	}

	got, err := s.Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Endpoint != "demo" || got.Method != "POST" || got.Path != "/stripe" || got.Query != "x=1" {
		t.Errorf("stored %+v", got)
	}
	if !bytes.Equal(got.Body, body) {
		t.Errorf("Body = %q, want %q", got.Body, body)
	}
	if want := strings.TrimPrefix(srv.URL, "http://"); len(got.Headers["Host"]) != 1 || got.Headers["Host"][0] != want {
		t.Errorf("Host header = %v, want %q", got.Headers["Host"], want)
	}
	if got.CreatedAt == 0 {
		t.Error("CreatedAt not set")
	}
}

func TestCaptureAnyMethod(t *testing.T) {
	srv, s := newTestServer(t, &Handler{})

	for i, method := range []string{"PUT", "PATCH", "DELETE", "GET"} {
		if status, _ := send(t, method, srv.URL+"/h/demo/x", nil, nil); status != http.StatusOK {
			t.Fatalf("%s: status %d", method, status)
		}
		got, err := s.Get(context.Background(), int64(i+1))
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Method != method {
			t.Errorf("Method = %q, want %q", got.Method, method)
		}
	}
}

func TestCapturePaths(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"/h/demo", "/"},
		{"/h/demo/", "/"},
		{"/h/demo/a/b", "/a/b"},
		{"/h/demo/a%3Fb", "/a%3Fb"}, // stays escaped, or forwarding would see a query
	}

	srv, s := newTestServer(t, &Handler{})

	for i, tt := range tests {
		if status, _ := send(t, "POST", srv.URL+tt.url, nil, nil); status != http.StatusOK {
			t.Fatalf("%s: status %d", tt.url, status)
		}
		got, err := s.Get(context.Background(), int64(i+1))
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Path != tt.want {
			t.Errorf("%s: Path = %q, want %q", tt.url, got.Path, tt.want)
		}
	}
}

func TestInvalidEndpoint(t *testing.T) {
	srv, s := newTestServer(t, &Handler{})

	for _, url := range []string{"/h/bad%20name/x", "/h/" + strings.Repeat("a", 65)} {
		if status, _ := send(t, "POST", srv.URL+url, nil, nil); status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", url, status)
		}
	}
	if n := storedCount(t, s); n != 0 {
		t.Errorf("stored %d rows, want 0", n)
	}
}

func TestBodyOverMaxBody(t *testing.T) {
	srv, s := newTestServer(t, &Handler{MaxBody: 10})

	status, _ := send(t, "POST", srv.URL+"/h/demo", make([]byte, 11), nil)
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", status)
	}
	if n := storedCount(t, s); n != 0 {
		t.Errorf("stored %d rows, want 0", n)
	}

	if status, _ := send(t, "POST", srv.URL+"/h/demo", make([]byte, 10), nil); status != http.StatusOK {
		t.Errorf("body at the limit: status %d, want 200", status)
	}
}

func TestRemoteIP(t *testing.T) {
	srv, s := newTestServer(t, &Handler{})
	ctx := context.Background()

	send(t, "POST", srv.URL+"/h/demo", nil, map[string]string{"CF-Connecting-IP": "203.0.113.9"})
	send(t, "POST", srv.URL+"/h/demo", nil, nil)

	withHeader, err := s.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if withHeader.RemoteIP != "203.0.113.9" {
		t.Errorf("RemoteIP = %q, want 203.0.113.9", withHeader.RemoteIP)
	}

	direct, err := s.Get(ctx, 2)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if direct.RemoteIP != "127.0.0.1" {
		t.Errorf("RemoteIP = %q, want 127.0.0.1", direct.RemoteIP)
	}
}

func TestClientIPIgnoresHeaderFromRemotePeer(t *testing.T) {
	r := httptest.NewRequest("POST", "/h/demo", nil)
	r.RemoteAddr = "198.51.100.7:1234"
	r.Header.Set("CF-Connecting-IP", "203.0.113.9")

	if got := clientIP(r); got != "198.51.100.7" {
		t.Errorf("clientIP = %q, want 198.51.100.7", got)
	}
}

func TestForwarderCalledOnce(t *testing.T) {
	forwarder := make(fakeForwarder, 2)
	srv, _ := newTestServer(t, &Handler{Forwarder: forwarder})

	send(t, "POST", srv.URL+"/h/demo/stripe", []byte("hello"), nil)

	select {
	case got := <-forwarder:
		if got.ID != 1 || got.Path != "/stripe" || string(got.Body) != "hello" {
			t.Errorf("forwarded %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Forward was not called")
	}

	select {
	case got := <-forwarder:
		t.Errorf("Forward called again with %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRespondStatusStillStores(t *testing.T) {
	srv, s := newTestServer(t, &Handler{Respond: http.StatusInternalServerError})

	status, _ := send(t, "POST", srv.URL+"/h/demo", nil, nil)
	if status != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", status)
	}
	if n := storedCount(t, s); n != 1 {
		t.Errorf("stored %d rows, want 1", n)
	}
}

func TestUnknownPath(t *testing.T) {
	srv, _ := newTestServer(t, &Handler{})

	status, reply := send(t, "GET", srv.URL+"/foo", nil, nil)
	if status != http.StatusNotFound || !strings.Contains(reply, "/h/<endpoint>") {
		t.Errorf("got %d %q", status, reply)
	}
}

func TestHealth(t *testing.T) {
	srv, _ := newTestServer(t, &Handler{})

	if status, reply := send(t, "GET", srv.URL+"/_health", nil, nil); status != http.StatusOK || reply != "ok" {
		t.Errorf("got %d %q", status, reply)
	}
}

func TestLogsOneLine(t *testing.T) {
	lines := make(chan string, 2)
	srv, _ := newTestServer(t, &Handler{Logf: func(format string, args ...any) {
		lines <- fmt.Sprintf(format, args...)
	}})

	send(t, "POST", srv.URL+"/h/demo/stripe?token=secret", []byte("hello"), nil)

	select {
	case got := <-lines:
		if want := "← #1 POST /stripe  5 B"; got != want {
			t.Errorf("log = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing logged")
	}
}
