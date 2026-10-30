package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mainak58/hooktape/internal/model"
	"github.com/mainak58/hooktape/internal/render"
	"github.com/mainak58/hooktape/internal/store"
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

func TestParseRetention(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"0", 0, false},
		{"", 0, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"1.5d", 36 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"24h", 24 * time.Hour, false},
		{"7days", 0, true},
		{"soon", 0, true},
	}

	for _, tt := range tests {
		got, err := parseRetention(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseRetention(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("parseRetention(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
