package tunnel

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const fake = "./testdata/fake-cloudflared"

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake cloudflared is a shell script")
	}
}

func TestParseQuickURL(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{"INF |  https://brave-otter.trycloudflare.com    |", "https://brave-otter.trycloudflare.com"},
		{"2026-10-30T12:00:00Z INF Visit it at https://odd-sheep-42.trycloudflare.com now", "https://odd-sheep-42.trycloudflare.com"},
		{"https://brave-otter.trycloudflare.com", "https://brave-otter.trycloudflare.com"},
		{"INF Starting tunnel tunnelID=abc", ""},
		{"http://brave-otter.trycloudflare.com", ""},  // not https
		{"https://brave-otter.example.com", ""},       // not a quick tunnel host
		{"https://BRAVE-OTTER.trycloudflare.com", ""}, // cloudflared prints lower case
		{"", ""},
	}

	for _, tt := range tests {
		got, ok := parseQuickURL(tt.line)
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("parseQuickURL(%q) = (%q, %v), want %q", tt.line, got, ok, tt.want)
		}
	}
}

func TestStartQuickTunnel(t *testing.T) {
	skipOnWindows(t)

	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	tn, errs, err := Start(context.Background(), Options{
		LocalURL: "http://127.0.0.1:8080",
		Binary:   fake,
		Logf:     logf,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if tn.PublicURL != "https://brave-otter.trycloudflare.com" {
		t.Errorf("PublicURL = %q", tn.PublicURL)
	}

	mu.Lock()
	logged := strings.Join(lines, "\n")
	mu.Unlock()
	if !strings.Contains(logged, "[cloudflared] INF fake cloudflared started with: tunnel --no-autoupdate --url http://127.0.0.1:8080") {
		t.Errorf("wrong arguments or no forwarded log lines:\n%s", logged)
	}

	// Still running until it is stopped.
	select {
	case err := <-errs:
		t.Fatalf("exited early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := tn.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case <-errs: // a signalled child reports an error, which is expected
	case <-time.After(3 * time.Second):
		t.Error("Stop did not reap cloudflared")
	}

	if err := tn.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

func TestStartNamedTunnel(t *testing.T) {
	skipOnWindows(t)

	tn, _, err := Start(context.Background(), Options{
		LocalURL:   "http://127.0.0.1:8080",
		TunnelName: "demo",
		Binary:     fake,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tn.Stop()

	// A named tunnel's hostname lives in Cloudflare's config, not in the log.
	if tn.PublicURL != "" {
		t.Errorf("PublicURL = %q, want empty", tn.PublicURL)
	}
}

func TestStartTimesOut(t *testing.T) {
	skipOnWindows(t)

	old := startTimeout
	startTimeout = 200 * time.Millisecond
	defer func() { startTimeout = old }()

	_, _, err := Start(context.Background(), Options{
		LocalURL: "http://127.0.0.1:8080",
		Binary:   fake + "-silent",
	})
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if !strings.Contains(err.Error(), "no tunnel within") {
		t.Errorf("err = %v", err)
	}
}

func TestStartBinaryNotFound(t *testing.T) {
	if _, _, err := Start(context.Background(), Options{Binary: "cloudflared-does-not-exist"}); err == nil {
		t.Fatal("want an error when the binary is missing")
	}
}
