package forward

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
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
			}, srv.URL, SendOptions{KeepPath: true})

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

	send(t, &model.Request{Method: "POST", Path: "/a%3Fb"}, srv.URL+"/", SendOptions{KeepPath: true})

	if app := <-got; app.path != "/a%3Fb" || app.query != "" {
		t.Errorf("app got path %q, query %q", app.path, app.query)
	}
}

func TestSendKeepPathWithTargetQuery(t *testing.T) {
	srv, got := newApp(t)

	send(t, &model.Request{Method: "POST", Path: "/charges", Query: "x=1"}, srv.URL+"/webhooks?key=abc", SendOptions{KeepPath: true})

	if app := <-got; app.path != "/webhooks/charges" || app.query != "key=abc&x=1" {
		t.Errorf("app got path %q, query %q", app.path, app.query)
	}
}

func TestSendKeepPathBareEndpointKeepsTarget(t *testing.T) {
	srv, got := newApp(t)

	send(t, &model.Request{Method: "POST", Path: "/"}, srv.URL+"/webhooks", SendOptions{KeepPath: true})

	if app := <-got; app.path != "/webhooks" {
		t.Errorf("app got path %q", app.path)
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
		"cf-lowercase", "Expect",
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

	res := send(t, &model.Request{Method: "POST", Path: "/from"}, srv.URL, SendOptions{KeepPath: true})

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

func TestSendIgnoresCallerPathByDefault(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		path      string
		query     string
		wantPath  string
		wantQuery string
	}{
		{"root", "", "/admin/users/1", "force=true", "/", "force=true"},
		{"base path", "/webhooks/stripe", "/admin", "", "/webhooks/stripe", ""},
		{"target with a query", "/hook?key=1", "/admin", "a=2", "/hook", "key=1&a=2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := newApp(t)

			send(t, &model.Request{Method: "DELETE", Path: tt.path, Query: tt.query}, srv.URL+tt.target, SendOptions{})

			if app := <-got; app.path != tt.wantPath || app.query != tt.wantQuery {
				t.Errorf("app got %q?%q, want %q?%q", app.path, app.query, tt.wantPath, tt.wantQuery)
			}
		})
	}
}

func TestSendRefusesDotDotPaths(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer srv.Close()

	for _, path := range []string{"/../x", "/a/../../x", "/%2e%2e/x", "/%2E%2e/x", "/.%2e/x", "/a/..%2Fx", "/..%5Cx"} {
		res := send(t, &model.Request{Method: "POST", Path: path}, srv.URL+"/base", SendOptions{KeepPath: true})
		if res.Err == nil {
			t.Errorf("%s: no error", path)
		}
	}
	if reached {
		t.Error("a dot-segment path reached the app")
	}

	if res := send(t, &model.Request{Method: "POST", Path: "/a..b/c.d"}, srv.URL, SendOptions{KeepPath: true}); res.Err != nil {
		t.Errorf("harmless dots refused: %v", res.Err)
	}
}

func TestSendRefusesTruncatedBody(t *testing.T) {
	srv, got := newApp(t)

	res := send(t, &model.Request{Method: "POST", Path: "/", Body: []byte("cut"), Truncated: true}, srv.URL, SendOptions{})

	if !errors.Is(res.Err, ErrTruncated) || res.Status != 0 {
		t.Errorf("Result = %+v, want ErrTruncated", res)
	}
	select {
	case app := <-got:
		t.Errorf("app got %+v", app)
	default:
	}
}

func TestSendCredentialsOnReplay(t *testing.T) {
	secrets := map[string][]string{"Authorization": {"Bearer x"}, "Cookie": {"s=1"}, "Stripe-Signature": {"v1=a"}}

	// The test server is on loopback, so a non-local target has to reach it
	// through a client that ignores the URL's host.
	remote := func(srv *httptest.Server) *http.Client {
		client := NewClient(5 * time.Second)
		client.Transport = &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		}}
		return client
	}

	tests := []struct {
		name     string
		remote   bool
		opts     SendOptions
		wantAuth bool
	}{
		{"local replay keeps them", false, SendOptions{Replay: true}, true},
		{"remote replay drops them", true, SendOptions{Replay: true}, false},
		{"remote replay with the opt-in", true, SendOptions{Replay: true, KeepCredentials: true}, true},
		{"live forward keeps them", true, SendOptions{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := newApp(t)

			client, target := NewClient(5*time.Second), srv.URL
			if tt.remote {
				client, target = remote(srv), "http://example.com"
			}
			Send(context.Background(), client, &model.Request{Method: "POST", Path: "/", Headers: secrets}, target, tt.opts)

			app := <-got
			if has := app.header.Get("Authorization") != "" && app.header.Get("Cookie") != ""; has != tt.wantAuth {
				t.Errorf("credentials present = %v, want %v", has, tt.wantAuth)
			}
			if app.header.Get("Stripe-Signature") == "" {
				t.Error("signature header was dropped")
			}
		})
	}
}

func TestLocalClientRefusesPublicAddresses(t *testing.T) {
	r := &model.Request{Method: "POST", Path: "/"}
	client := NewLocalClient(2 * time.Second)

	srv, got := newApp(t)
	if res := Send(context.Background(), client, r, srv.URL, SendOptions{}); res.Err != nil || res.Status != http.StatusOK {
		t.Fatalf("loopback: %+v", res)
	}
	<-got

	for _, target := range []string{"http://169.254.169.254", "http://8.8.8.8", "http://[2001:4860:4860::8888]", "http://0.0.0.0:1"} {
		res := Send(context.Background(), client, r, target, SendOptions{})
		if res.Err == nil || !strings.Contains(res.Err.Error(), "refusing to connect") {
			t.Errorf("%s: err = %v, want a refusal", target, res.Err)
		}
	}
}

func TestLocalClientIgnoresProxyEnv(t *testing.T) {
	transport := NewLocalClient(time.Second).Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Error("Proxy is set, so the dial check would only see the proxy")
	}
}

func TestIsLocalTarget(t *testing.T) {
	tests := []struct {
		target string
		want   bool
	}{
		{"http://localhost:3000", true},
		{"http://LOCALHOST", true},
		{"http://127.0.0.1:3000/x", true},
		{"http://[::1]:3000", true},
		{"http://10.0.0.5", true},
		{"http://192.168.1.2:80", true},
		{"http://169.254.169.254", false},
		{"http://8.8.8.8", false},
		{"https://example.com", false},
		{"http://localhost.evil.example", false},
		{"http://bad host", false},
	}

	for _, tt := range tests {
		if got := IsLocalTarget(tt.target); got != tt.want {
			t.Errorf("IsLocalTarget(%q) = %v, want %v", tt.target, got, tt.want)
		}
	}
}

func TestCheckTarget(t *testing.T) {
	if got, err := CheckTarget("  http://localhost:3000 "); err != nil || got != "http://localhost:3000" {
		t.Errorf("got %q, %v", got, err)
	}

	for _, target := range []string{"", "   ", "localhost:3000", "file:///etc/passwd", "gopher://x", "http://"} {
		if _, err := CheckTarget(target); err == nil {
			t.Errorf("CheckTarget(%q) passed", target)
		}
	}
}
