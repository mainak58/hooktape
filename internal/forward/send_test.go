package forward

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mainak58/hooktape/internal/model"
	"github.com/mainak58/hooktape/internal/render"
)

type received struct {
	method        string
	path          string // as sent, still escaped
	query         string
	host          string
	contentLength int64
	header        http.Header
	body          []byte
}

// newApp starts a fake app that records each request it gets and answers 200.
func newApp(t *testing.T) (*httptest.Server, <-chan received) {
	t.Helper()
	got := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		got <- received{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Host, r.ContentLength, r.Header.Clone(), body}
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func send(t *testing.T, r *model.Request, target string, opts SendOptions) Result {
	t.Helper()
	return Send(context.Background(), NewClient(5*time.Second), r, target, opts)
}

func gzipped(t *testing.T, data string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSendDeliversRequest(t *testing.T) {
	tests := []struct {
		name    string
		body    []byte
		headers map[string][]string
	}{
		{"json", []byte(`{"type":"charge.succeeded"}`), map[string][]string{"Content-Type": {"application/json"}}},
		{"invalid utf-8", []byte{0xff, 0x00, 0xfe, 'a'}, nil},
		{"gzip stays compressed", gzipped(t, "hello hello hello"), map[string][]string{"Content-Encoding": {"gzip"}}},
		{"empty", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := newApp(t)

			res := send(t, &model.Request{
				ID:      7,
				Method:  "PUT",
				Path:    "/stripe/charge",
				Query:   "x=1&y=2",
				Headers: tt.headers,
				Body:    tt.body,
			}, srv.URL, SendOptions{})

			if res.Err != nil || res.Status != http.StatusOK {
				t.Fatalf("Result = %+v", res)
			}
			if res.Duration <= 0 {
				t.Errorf("Duration = %v", res.Duration)
			}

			app := <-got
			if app.method != "PUT" || app.path != "/stripe/charge" || app.query != "x=1&y=2" {
				t.Errorf("app got %s %s?%s", app.method, app.path, app.query)
			}
			if !bytes.Equal(app.body, tt.body) {
				t.Errorf("body = %x, want %x", app.body, tt.body)
			}
			if app.contentLength != int64(len(tt.body)) {
				t.Errorf("Content-Length = %d, want %d", app.contentLength, len(tt.body))
			}
			for name, want := range tt.headers {
				if got := app.header.Get(name); got != want[0] {
					t.Errorf("%s = %q, want %q", name, got, want[0])
				}
			}
		})
	}
}

func TestSendTrimsTargetSlashAndKeepsEscapedPath(t *testing.T) {
	srv, got := newApp(t)

	send(t, &model.Request{Method: "POST", Path: "/a%3Fb"}, srv.URL+"/", SendOptions{})

	if app := <-got; app.path != "/a%3Fb" || app.query != "" {
		t.Errorf("app got path %q, query %q", app.path, app.query)
	}
}

func TestSendKeepsRepeatedHeaders(t *testing.T) {
	srv, got := newApp(t)

	send(t, &model.Request{
		Method:  "POST",
		Path:    "/",
		Headers: map[string][]string{"X-Multi": {"a", "b"}},
	}, srv.URL, SendOptions{})

	if values := (<-got).header.Values("X-Multi"); len(values) != 2 || values[0] != "a" || values[1] != "b" {
		t.Errorf("X-Multi = %v", values)
	}
}

func TestSendDropsHeaders(t *testing.T) {
	srv, got := newApp(t)

	headers := map[string][]string{"X-Keep": {"yes"}}
	for _, name := range []string{
		"Host", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Upgrade",
		"Proxy-Authorization", "Proxy-Authenticate", "Cf-Connecting-Ip", "Cf-Ray",
		"cf-lowercase",
	} {
		headers[name] = []string{"dropped"}
	}
	headers["Accept-Encoding"] = []string{"br"}

	send(t, &model.Request{Method: "POST", Path: "/", Headers: headers}, srv.URL, SendOptions{})

	app := <-got
	for name := range headers {
		if name == "X-Keep" {
			continue
		}
		for _, value := range app.header.Values(name) {
			if value == "dropped" || value == "br" {
				t.Errorf("%s reached the app", name)
			}
		}
	}
	if app.header.Get("X-Keep") != "yes" {
		t.Error("X-Keep was dropped")
	}
	if want := strings.TrimPrefix(srv.URL, "http://"); app.host != want {
		t.Errorf("Host = %q, want %q", app.host, want)
	}
}

func TestSendHooktapeHeaders(t *testing.T) {
	// Both are faked by the sender, so neither may survive as sent.
	faked := map[string][]string{"X-Hooktape-Id": {"999"}, "X-Hooktape-Replay": {"true"}}

	tests := []struct {
		name       string
		opts       SendOptions
		wantReplay string
	}{
		{"normal", SendOptions{}, ""},
		{"replay", SendOptions{Replay: true}, "true"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := newApp(t)

			send(t, &model.Request{ID: 12, Method: "POST", Path: "/", Headers: faked}, srv.URL, tt.opts)

			app := <-got
			if id := app.header.Values("X-Hooktape-Id"); len(id) != 1 || id[0] != "12" {
				t.Errorf("X-Hooktape-Id = %v, want [12]", id)
			}
			if replay := app.header.Get("X-Hooktape-Replay"); replay != tt.wantReplay {
				t.Errorf("X-Hooktape-Replay = %q, want %q", replay, tt.wantReplay)
			}
		})
	}
}

func TestSendDoesNotFollowRedirects(t *testing.T) {
	var followed bool
	mux := http.NewServeMux()
	mux.HandleFunc("/from", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/to", http.StatusFound)
	})
	mux.HandleFunc("/to", func(w http.ResponseWriter, r *http.Request) {
		followed = true
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res := send(t, &model.Request{Method: "POST", Path: "/from"}, srv.URL, SendOptions{})

	if res.Err != nil || res.Status != http.StatusFound {
		t.Errorf("Result = %+v, want 302", res)
	}
	if followed {
		t.Error("redirect was followed")
	}
}

func TestSendAppDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	target := srv.URL
	srv.Close()

	res := send(t, &model.Request{Method: "POST", Path: "/"}, target, SendOptions{})

	if res.Err == nil || res.Status != 0 {
		t.Fatalf("Result = %+v, want an error and status 0", res)
	}
	if label := render.ForwardLabel(res.Status, res.Err.Error()); label != "refused" {
		t.Errorf("label = %q, want refused (%v)", label, res.Err)
	}
}

func TestSendTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	res := Send(context.Background(), NewClient(50*time.Millisecond), &model.Request{Method: "POST", Path: "/"}, srv.URL, SendOptions{})

	if res.Err == nil || res.Status != 0 {
		t.Fatalf("Result = %+v, want an error and status 0", res)
	}
	if label := render.ForwardLabel(res.Status, res.Err.Error()); label != "timeout" {
		t.Errorf("label = %q, want timeout (%v)", label, res.Err)
	}
}

func TestSendCapsResponseBody(t *testing.T) {
	tests := []struct {
		size int
		want int
	}{
		{5, 5},
		{64 << 10, 64 << 10},
		{1 << 20, 64 << 10},
	}

	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.size), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write(make([]byte, tt.size))
			}))
			defer srv.Close()

			res := send(t, &model.Request{Method: "POST", Path: "/"}, srv.URL, SendOptions{})

			if res.Err != nil || len(res.Body) != tt.want {
				t.Errorf("err = %v, len(Body) = %d, want %d", res.Err, len(res.Body), tt.want)
			}
		})
	}
}

func TestSendBadTarget(t *testing.T) {
	res := send(t, &model.Request{Method: "POST", Path: "/"}, "http://bad host", SendOptions{})

	if res.Err == nil || res.Status != 0 {
		t.Errorf("Result = %+v, want an error and status 0", res)
	}
}
