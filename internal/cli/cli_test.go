package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mainak58/hooktape/internal/model"
	"github.com/mainak58/hooktape/internal/render"
	"github.com/mainak58/hooktape/internal/store"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// run executes the command in-process. rootCmd is shared and cobra keeps flag
// values between runs, so every flag goes back to its default first.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()

	reset := func(f *pflag.Flag) {
		f.Value.Set(f.DefValue)
		f.Changed = false
	}
	rootCmd.PersistentFlags().VisitAll(reset)
	for _, c := range []*cobra.Command{listCmd, showCmd} {
		c.Flags().VisitAll(reset)
	}

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&bytes.Buffer{})
	rootCmd.SetArgs(args)
	err := rootCmd.Execute()
	return out.String(), err
}

// seed stores three requests and returns the db path: #1 forwarded with a 200,
// #2 with a 500, #3 refused.
func seed(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	rows := []struct {
		endpoint string
		status   int
		errMsg   string
	}{
		{"stripe", 200, ""},
		{"github", 500, ""},
		{"stripe", 0, "dial tcp 127.0.0.1:3000: connect: connection refused"},
	}
	for _, row := range rows {
		r := &model.Request{
			Endpoint:  row.endpoint,
			Method:    "POST",
			Path:      "/hook",
			Query:     "x=1",
			Headers:   map[string][]string{"Content-Type": {"application/json"}, "Accept": {"a", "b"}},
			Body:      []byte{0xff, 0x00, 0xfe, 'a'},
			CreatedAt: time.Now().UnixMilli(),
		}
		id, err := s.Insert(ctx, r)
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if err := s.SetForwardResult(ctx, id, row.status, 38, row.errMsg); err != nil {
			t.Fatalf("SetForwardResult: %v", err)
		}
	}
	return path
}

func TestListOutput(t *testing.T) {
	out, err := run(t, "list", "--db", seed(t))
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}

	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "ID TIME ENDPOINT METHOD PATH SIZE APP" {
		t.Errorf("header = %q", lines[0])
	}

	// Newest first, so row #3 (refused) comes straight after the header.
	for i, want := range []struct {
		id, endpoint, app string
	}{
		{"3", "stripe", "refused"},
		{"2", "github", "500"},
		{"1", "stripe", "200"},
	} {
		fields := strings.Fields(lines[i+1])
		if len(fields) != 8 { // SIZE is two words: "4 B"
			t.Fatalf("line %d: %q", i+1, lines[i+1])
		}
		if fields[0] != want.id || fields[2] != want.endpoint || fields[3] != "POST" ||
			fields[4] != "/hook" || fields[5] != "4" || fields[6] != "B" || fields[7] != want.app {
			t.Errorf("line %d = %q, want %+v", i+1, lines[i+1], want)
		}
	}

	if strings.Contains(out, "\033") {
		t.Errorf("colour codes written to a non-terminal: %q", out)
	}
}

func TestListEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	out, err := run(t, "list", "--db", path)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if want := "No requests yet. Run \"hooktape up\" and send a webhook to /h/<endpoint>/...\n"; out != want {
		t.Errorf("got %q, want %q", out, want)
	}

	out, err = run(t, "list", "--db", path, "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("--json on an empty db = %q, want []", out)
	}
}

func TestListFlags(t *testing.T) {
	path := seed(t)

	out, err := run(t, "list", "--db", path, "--limit", "1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if n := strings.Count(strings.TrimSpace(out), "\n"); n != 1 {
		t.Errorf("--limit 1 printed %d rows:\n%s", n, out)
	}

	out, err = run(t, "list", "--db", path, "--endpoint", "github")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(out, "stripe") || !strings.Contains(out, "github") {
		t.Errorf("--endpoint github:\n%s", out)
	}
}

func TestListJSONHasNoBodies(t *testing.T) {
	out, err := run(t, "list", "--db", seed(t), "--json")
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	if _, ok := rows[0]["body"]; ok {
		t.Errorf("row has a body: %v", rows[0])
	}
	if rows[0]["body_size"] != float64(4) {
		t.Errorf("body_size = %v, want 4", rows[0]["body_size"])
	}
}

func TestShowRawWritesExactBytes(t *testing.T) {
	out, err := run(t, "show", "1", "--db", seed(t), "--raw")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if want := []byte{0xff, 0x00, 0xfe, 'a'}; !bytes.Equal([]byte(out), want) {
		t.Errorf("got %x, want %x", out, want)
	}
}

func TestShowOutput(t *testing.T) {
	path := seed(t)

	out, err := run(t, "show", "2", "--db", path)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{
		"POST /hook?x=1  (#2, endpoint github, 4 B, ",
		"Accept: a\nAccept: b\nContent-Type: application/json\n\n",
		"<4 bytes binary, first 32 bytes hex: ff00fe61>\n",
		"app: 500 in 38ms\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	out, err = run(t, "show", "3", "--db", path)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if want := "app: refused (dial tcp 127.0.0.1:3000: connect: connection refused)\n"; !strings.HasSuffix(out, want) {
		t.Errorf("want suffix %q in:\n%s", want, out)
	}
}

func TestShowPartialFlags(t *testing.T) {
	path := seed(t)

	out, err := run(t, "show", "1", "--db", path, "--headers-only")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if want := "Accept: a\nAccept: b\nContent-Type: application/json\n"; out != want {
		t.Errorf("--headers-only = %q, want %q", out, want)
	}

	out, err = run(t, "show", "1", "--db", path, "--body-only")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if want := "<4 bytes binary, first 32 bytes hex: ff00fe61>\n"; out != want {
		t.Errorf("--body-only = %q, want %q", out, want)
	}
}

func TestShowErrors(t *testing.T) {
	path := seed(t)

	if _, err := run(t, "show", "99", "--db", path); err == nil {
		t.Error("unknown id: want error")
	}
	if _, err := run(t, "show", "abc", "--db", path); err == nil {
		t.Error("non-numeric id: want error")
	}
	if _, err := run(t, "show", "1", "--db", path, "--raw", "--body-only"); err == nil {
		t.Error("--raw with --body-only: want error")
	}
}

func TestFormatTime(t *testing.T) {
	now := time.Date(2026, time.March, 5, 14, 30, 0, 0, time.Local)

	tests := []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, time.March, 5, 14, 2, 11, 0, time.Local), "14:02:11"},
		{time.Date(2026, time.March, 4, 14, 2, 11, 0, time.Local), "Mar 04 14:02"},
		{time.Date(2025, time.March, 5, 14, 2, 11, 0, time.Local), "Mar 05 14:02"},
	}

	for _, tt := range tests {
		if got := formatTime(tt.at.UnixMilli(), now); got != tt.want {
			t.Errorf("formatTime(%v) = %q, want %q", tt.at, got, tt.want)
		}
	}
}

func TestAppLabelColours(t *testing.T) {
	tests := []struct {
		status int
		errMsg string
		code   string
	}{
		{200, "", "32"},
		{302, "", "33"},
		{404, "", "31"},
		{500, "", "31"},
		{0, "connection refused", "31"},
		{0, "", "2"},
	}

	for _, tt := range tests {
		name := strconv.Itoa(tt.status) + tt.errMsg
		got := appLabel(render.Colors(true), &model.Request{ForwardStatus: tt.status, ForwardError: tt.errMsg})
		if !strings.HasPrefix(got, "\033["+tt.code+"m") {
			t.Errorf("%s: got %q, want colour %s", name, got, tt.code)
		}
	}
}
