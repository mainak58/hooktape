package cli

import (
	"context"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mainak58/hooktape/internal/model"
	"github.com/mainak58/hooktape/internal/store"
)

// dated stores one row per timestamp and returns the db path.
func dated(t *testing.T, at ...time.Time) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	for _, when := range at {
		_, err := s.Insert(context.Background(), &model.Request{
			Endpoint: "stripe", Method: "POST", Path: "/", Headers: map[string][]string{}, CreatedAt: when.UnixMilli(),
		})
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	return path
}

func remaining(t *testing.T, path string) []int64 {
	t.Helper()

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	rows, err := s.List(context.Background(), store.ListOptions{Limit: 1000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	ids := []int64{}
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)

	return ids
}

func TestDeleteRangeBoundaries(t *testing.T) {
	local := func(month time.Month, day, hour, minute, second, ms int) time.Time {
		return time.Date(2026, month, day, hour, minute, second, ms*int(time.Millisecond), time.Local)
	}
	at := []time.Time{
		local(time.January, 1, 0, 0, 0, 0).Add(-time.Millisecond), // #1 just before --from
		local(time.January, 1, 0, 0, 0, 0),                        // #2 first instant of --from
		local(time.March, 31, 23, 59, 59, 999),                    // #3 last instant of the --to day
		local(time.April, 1, 0, 0, 0, 0),                          // #4 first instant after it
	}

	tests := []struct {
		name string
		args []string
		want []int64
	}{
		{"date range covers the whole --to day", []string{"--from", "2026-01-01", "--to", "2026-03-31"}, []int64{1, 4}},
		{"--to alone", []string{"--to", "2026-03-31"}, []int64{4}},
		{"--from alone", []string{"--from", "2026-01-01"}, []int64{1}},
		{"--to with a time includes that instant", []string{"--to", "2026-01-01 00:00"}, []int64{3, 4}},
		{"--to with a time stops there", []string{"--to", "2026-03-31 23:59:59"}, []int64{3, 4}},
		{"--from with a time", []string{"--from", "2026-04-01T00:00", "--to", "2026-04-01T00:00"}, []int64{1, 2, 3}},
		{"other endpoint matches nothing", []string{"--endpoint", "github", "--to", "2026-03-31"}, []int64{1, 2, 3, 4}},
		{"endpoint narrows", []string{"--endpoint", "stripe", "--from", "2026-04-01"}, []int64{1, 2, 3}},
		{"endpoint alone", []string{"--endpoint", "stripe"}, []int64{}},
		{"all", []string{"--all"}, []int64{}},
		{"ids", []string{"1", "4", "4", "99"}, []int64{2, 3}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := dated(t, at...)

			args := append([]string{"delete", "--db", path, "--yes"}, tt.args...)
			if out, err := run(t, args...); err != nil {
				t.Fatalf("delete: %v\n%s", err, out)
			}
			if got := remaining(t, path); !slices.Equal(got, tt.want) {
				t.Errorf("left %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeleteOlderThan(t *testing.T) {
	now := time.Now()
	path := dated(t, now.Add(-49*time.Hour), now.Add(-47*time.Hour))

	if out, err := run(t, "delete", "--db", path, "--yes", "--older-than", "2d"); err != nil {
		t.Fatalf("delete: %v\n%s", err, out)
	}
	if got := remaining(t, path); !slices.Equal(got, []int64{2}) {
		t.Errorf("left %v, want [2]", got)
	}
}

// A day can be 23 hours long, and a fixed 24h bound would stop short of it.
func TestDeleteToDateSurvivesClockChange(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no time zone data:", err)
	}

	old := time.Local
	time.Local = newYork
	t.Cleanup(func() { time.Local = old })

	if _, err := run(t, "version"); err != nil { // clears flags left by earlier tests
		t.Fatal(err)
	}
	deleteTo = "2026-03-08" // clocks go forward that morning
	t.Cleanup(func() { deleteTo = "" })

	_, filter, _, err := deleteSelection(nil, time.Now())
	if err != nil {
		t.Fatalf("deleteSelection: %v", err)
	}
	if want := time.Date(2026, time.March, 9, 0, 0, 0, 0, newYork).UnixMilli(); filter.ToMs != want {
		t.Errorf("ToMs = %d, want %d", filter.ToMs, want)
	}
}

func TestDeleteRefusals(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"nothing selected", nil, "nothing selected"},
		{"empty values select nothing", []string{"--from", "", "--to", ""}, "is empty"},
		{"ids and filter", []string{"1", "--from", "2026-01-01"}, "ids cannot be combined"},
		{"ids and all", []string{"1", "--all"}, "ids cannot be combined"},
		{"all and from", []string{"--all", "--from", "2026-01-01"}, "--all cannot be combined"},
		{"all and endpoint", []string{"--all", "--endpoint", "x"}, "--all cannot be combined"},
		{"older-than and to", []string{"--older-than", "7d", "--to", "2026-01-01"}, "--older-than cannot be combined"},
		{"older-than and from", []string{"--older-than", "7d", "--from", "2026-01-01"}, "--older-than cannot be combined"},
		{"from after to", []string{"--from", "2026-02-01", "--to", "2026-01-01"}, "is after"},
		{"bad date", []string{"--from", "yesterday"}, "invalid --from"},
		{"impossible date", []string{"--to", "2026-02-30"}, "invalid --to"},
		{"bad id", []string{"abc"}, "invalid id"},
		{"empty endpoint must not widen", []string{"--endpoint", "", "--older-than", "1h"}, "--endpoint is empty"},
		{"empty to must not lose its bound", []string{"--from", "2000-01-01", "--to", ""}, "--to is empty"},
		{"age that truncates to zero", []string{"--older-than", "1e-20d"}, "invalid --older-than"},
		{"to at the epoch would widen to everything", []string{"--to", "1969-12-31T23:59:59.999Z"}, "before 1970"},
		{"from at the epoch", []string{"--from", "1970-01-01T00:00:00Z"}, "before 1970"},
		{"older than time itself", []string{"--older-than", "100000d"}, "before 1970"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := seed(t)

			args := append([]string{"delete", "--db", path, "--yes"}, tt.args...)
			_, err := run(t, args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
			if got := remaining(t, path); len(got) != 3 {
				t.Errorf("a refused delete removed rows: %v left", got)
			}
		})
	}
}

func TestDeletePrompt(t *testing.T) {
	tests := []struct {
		name, input string
		wantLeft    int
		wantErr     bool
	}{
		{"yes", "y\n", 0, false},
		{"word", "YES\n", 0, false},
		{"no", "n\n", 3, true},
		{"enter", "\n", 3, true},
		{"eof", "", 3, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdin = tt.input
			t.Cleanup(func() { stdin = "" })
			path := seed(t)

			out, err := run(t, "delete", "--db", path, "--all")
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !strings.Contains(out, "Delete 3 request(s) (everything)? [y/N] ") {
				t.Errorf("no prompt in %q", out)
			}
			if got := remaining(t, path); len(got) != tt.wantLeft {
				t.Errorf("left %v, want %d rows", got, tt.wantLeft)
			}
		})
	}
}

func TestDeleteDryRun(t *testing.T) {
	path := seed(t)

	out, err := run(t, "delete", "--db", path, "--all", "--dry-run")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(out, "Would delete 3 request(s) (everything)") {
		t.Errorf("got %q", out)
	}
	if got := remaining(t, path); len(got) != 3 {
		t.Errorf("--dry-run removed rows: %v left", got)
	}
}

func TestDeleteZeroMatches(t *testing.T) {
	path := seed(t)

	// Empty stdin would abort a prompt, so success proves it was never shown.
	out, err := run(t, "delete", "--db", path, "--to", "2020-01-01")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(out, "Nothing to delete") || strings.Contains(out, "[y/N]") {
		t.Errorf("got %q", out)
	}
}

func TestParseAge(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"1.5d", 36 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"24h", 24 * time.Hour, false},
		{"", 0, true},
		{"0", 0, true},
		{"0d", 0, true},
		{"-7d", 0, true},
		{"-1h", 0, true},
		{"NaNd", 0, true},
		{"Infd", 0, true},
		{"1.1e5d", 0, true},
		{"7days", 0, true},
		{"soon", 0, true},
	}

	for _, tt := range tests {
		got, err := parseAge(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseAge(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("parseAge(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}

	if got := float64(106751 * 24 * time.Hour); got >= math.MaxInt64 {
		t.Fatalf("test value overflowed: %v", got)
	}
	if _, err := parseAge("106751d"); err != nil {
		t.Errorf("largest whole day that fits: %v", err)
	}
}
