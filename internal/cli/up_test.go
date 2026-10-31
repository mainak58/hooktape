package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mainak58/hooktape/internal/model"
	"github.com/mainak58/hooktape/internal/render"
	"github.com/mainak58/hooktape/internal/store"
	"github.com/spf13/cobra"
)

// logSink collects log lines from the worker goroutine.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (l *logSink) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func TestForwarderRecordsResultInOrder(t *testing.T) {
	replayEnv(t)
	path := seed(t)
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var mu sync.Mutex
	var sent []string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Header.Get("X-Hooktape-Id"))
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer app.Close()

	var sink logSink
	f := newForwarder(s, app.URL, 5*time.Second, render.Colors(false), sink.logf)
	f.start()

	for _, id := range []int64{3, 1, 2} {
		f.Forward(&model.Request{ID: id, Method: "POST", Path: "/hook"})
	}
	f.stop(5 * time.Second)

	if got := strings.Join(sent, ","); got != "3,1,2" {
		t.Errorf("app saw %q, want 3,1,2", got)
	}

	r, err := s.Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.ForwardStatus != 500 {
		t.Errorf("stored status = %d, want 500", r.ForwardStatus)
	}

	if !strings.Contains(sink.text(), "→ #1 POST /hook  500 ") {
		t.Errorf("missing forward log line:\n%s", sink.text())
	}
}

func TestForwarderDropsWhenQueueFull(t *testing.T) {
	var sink logSink

	// No worker started and no room in the queue, so the next Forward must drop
	// rather than block the catcher.
	f := newForwarder(nil, "http://example.invalid", time.Second, render.Colors(false), sink.logf)
	f.queue = make(chan *model.Request)

	done := make(chan struct{})
	go func() {
		f.Forward(&model.Request{ID: 7})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Forward blocked on a full queue")
	}

	if want := "forward queue full, #7 saved but not forwarded (use hooktape replay 7)"; !strings.Contains(sink.text(), want) {
		t.Errorf("got %q, want %q", sink.text(), want)
	}
}

func TestForwardAfterStopDoesNotPanic(t *testing.T) {
	var sink logSink
	f := newForwarder(nil, "http://example.invalid", time.Second, render.Colors(false), sink.logf)
	f.start()
	f.stop(time.Second)
	f.stop(time.Second)

	f.Forward(&model.Request{ID: 9})

	if want := "#9 saved but not forwarded"; !strings.Contains(sink.text(), want) {
		t.Errorf("got %q, want %q", sink.text(), want)
	}
}

func TestStopNamesEveryUnsentRequest(t *testing.T) {
	replayEnv(t)

	release := make(chan struct{})
	reached := make(chan struct{})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(reached)
		<-release
	}))
	defer app.Close()

	path := seed(t)
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var sink logSink
	f := newForwarder(s, app.URL, 5*time.Second, render.Colors(false), sink.logf)
	f.start()
	f.Forward(&model.Request{ID: 1, Method: "POST"})
	<-reached
	f.Forward(&model.Request{ID: 2, Method: "POST"})
	f.Forward(&model.Request{ID: 3, Method: "POST"})

	f.stop(10 * time.Millisecond)
	close(release)
	<-f.done

	got := sink.text()
	for _, want := range []string{"gave up draining", " 1", " 2", " 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestUpRejectsBadValues(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"respond low", []string{"--respond", "99"}, "invalid --respond"},
		{"respond high", []string{"--respond", "600"}, "invalid --respond"},
		{"forward scheme", []string{"--forward", "localhost:3000"}, "--forward: invalid target"},
		{"workers", []string{"--forward-workers", "0"}, "invalid --forward-workers"},
		{"tunnel name", []string{"--tunnel-name=-x"}, "invalid --tunnel-name"},
		{"same ports", []string{"--port", "18555", "--ui-port", "18555"}, "both 18555"},
		{"same ports on different addresses", []string{"--addr", "0.0.0.0", "--port", "18555", "--ui-port", "18555"}, "both 18555"},
		{"respond interim", []string{"--respond", "103"}, "invalid --respond"},
		{"max body zero", []string{"--max-body", "0"}, "invalid --max-body"},
		{"max body overflow", []string{"--max-body", "10EB"}, "invalid --max-body"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"up", "--no-tunnel", "--db", filepath.Join(t.TempDir(), "t.db")}, tt.args...)
			_, err := run(t, args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRetentionIsGone(t *testing.T) {
	_, err := run(t, "up", "--no-tunnel", "--retention", "7d")
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("err = %v, want unknown flag", err)
	}
}

func TestLocalURL(t *testing.T) {
	tests := []struct {
		addr string
		want string
	}{
		{"127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"0.0.0.0:8080", "http://127.0.0.1:8080"}, // cloudflared needs a real interface
		{"[::]:8080", "http://127.0.0.1:8080"},    // same for the v6 wildcard
		{"192.168.1.5:9000", "http://192.168.1.5:9000"},
		{"[::1]:8080", "http://[::1]:8080"},
	}

	for _, tt := range tests {
		if got := localURL(fakeAddr(tt.addr)); got != tt.want {
			t.Errorf("localURL(%q) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}

func TestListenError(t *testing.T) {
	bind := func(err error) error {
		return &net.OpError{Op: "listen", Net: "tcp", Err: &os.SyscallError{Syscall: "bind", Err: err}}
	}

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"wrapped in use", bind(syscall.EADDRINUSE), "port 8080 is busy; try --port 8081"},
		{"bare in use", syscall.EADDRINUSE, "port 8080 is busy; try --port 8081"},
		{"permission denied", bind(syscall.EACCES), bind(syscall.EACCES).Error()},
		{"other", errors.New("no such host"), "no such host"},
	}

	for _, tt := range tests {
		if got := listenError(tt.err, 8080).Error(); got != tt.want {
			t.Errorf("%s: listenError = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestBannerPasteHintWithoutTunnel(t *testing.T) {
	upNoTunnel, upHint = true, "stripe"
	t.Cleanup(func() { upNoTunnel, upHint = false, "demo" })

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	printBanner(cmd, render.Colors(false), fakeAddr("0.0.0.0:8080"), nil)

	if want := "  paste →   http://127.0.0.1:8080/h/stripe\n"; !strings.Contains(out.String(), want) {
		t.Errorf("missing %q in:\n%s", want, out.String())
	}
}

func TestBannerShowsConfiguredAddress(t *testing.T) {
	upNoTunnel, upAddr = true, "0.0.0.0"
	t.Cleanup(func() { upNoTunnel, upAddr = false, "127.0.0.1" })

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	printBanner(cmd, render.Colors(false), fakeAddr("[::]:8080"), nil)

	for _, want := range []string{"http://0.0.0.0:8080/h/<endpoint>", "anyone on this network can reach the catcher"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }
