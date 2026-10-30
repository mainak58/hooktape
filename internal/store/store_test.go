package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/mainak58/hooktape/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insertRequest(t *testing.T, s *Store, endpoint string, body []byte) int64 {
	t.Helper()
	id, err := s.Insert(context.Background(), &model.Request{
		Endpoint: endpoint,
		Method:   "POST",
		Path:     "/",
		Headers:  map[string][]string{},
		Body:     body,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	return id
}

func TestOpenCreatesFileAndSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "test.db")

	// Open twice: the second open must accept the existing file and schema.
	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { s.db.Close() })

		if _, err := os.Stat(path); err != nil {
			t.Fatalf("db file not created: %v", err)
		}

		var table string
		err = s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'requests'`).Scan(&table)
		if err != nil {
			t.Fatalf("requests table missing: %v", err)
		}

		var journalMode string
		if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
			t.Fatalf("read journal_mode: %v", err)
		}
		if journalMode != "wal" {
			t.Errorf("journal_mode = %q, want wal", journalMode)
		}

		var busyTimeout int
		if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			t.Fatalf("read busy_timeout: %v", err)
		}
		if busyTimeout != 5000 {
			t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
		}
	}
}

func TestOpenFailsWhenParentIsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(filepath.Join(file, "test.db")); err == nil {
		t.Fatal("Open succeeded, want error")
	}
}

func TestInsertGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	r := &model.Request{
		Endpoint:  "demo",
		Method:    "POST",
		Path:      "/stripe",
		Query:     "x=1&y=2",
		Headers:   map[string][]string{"Host": {"example.com"}, "X-Multi": {"a", "b"}},
		Body:      []byte{0xff, 0x00, 0xfe, 'a'}, // invalid UTF-8 with a NUL byte
		RemoteIP:  "203.0.113.9",
		CreatedAt: 1700000000123,
	}

	id, err := s.Insert(ctx, r)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id == 0 || r.ID != id {
		t.Fatalf("Insert returned id %d and set r.ID to %d", id, r.ID)
	}

	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	want := *r
	want.BodySize = len(r.Body)
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("Get = %+v, want %+v", *got, want)
	}
}

func TestInsertGetEmptyBody(t *testing.T) {
	s := newTestStore(t)

	id := insertRequest(t, s, "demo", nil)

	got, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Body) != 0 || got.BodySize != 0 {
		t.Errorf("Body = %v, BodySize = %d, want both empty", got.Body, got.BodySize)
	}
}

func TestGetNotFound(t *testing.T) {
	s := newTestStore(t)

	got, err := s.Get(context.Background(), 99)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got != nil {
		t.Errorf("Get = %+v, want nil", got)
	}
}

func TestListNewestFirstAndLimit(t *testing.T) {
	s := newTestStore(t)
	for range 5 {
		insertRequest(t, s, "demo", nil)
	}

	got, err := s.List(context.Background(), ListOptions{Limit: 3})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var ids []int64
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if want := []int64{5, 4, 3}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}

func TestListDefaultLimit(t *testing.T) {
	s := newTestStore(t)
	for range 51 {
		insertRequest(t, s, "demo", nil)
	}

	got, err := s.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 50 {
		t.Errorf("len = %d, want 50", len(got))
	}
}

func TestListFilterEndpoint(t *testing.T) {
	s := newTestStore(t)
	insertRequest(t, s, "stripe", nil)
	want := insertRequest(t, s, "github", nil)
	insertRequest(t, s, "stripe", nil)

	got, err := s.List(context.Background(), ListOptions{Endpoint: "github"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != want {
		t.Errorf("List = %+v, want only id %d", got, want)
	}
}

func TestListWithoutBodyReportsSize(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	body := []byte("hello")
	insertRequest(t, s, "demo", body)

	got, err := s.List(ctx, ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got[0].Body != nil || got[0].BodySize != len(body) {
		t.Errorf("Body = %q, BodySize = %d, want nil and %d", got[0].Body, got[0].BodySize, len(body))
	}

	got, err = s.List(ctx, ListOptions{WithBody: true})
	if err != nil {
		t.Fatalf("List WithBody: %v", err)
	}
	if !bytes.Equal(got[0].Body, body) || got[0].BodySize != len(body) {
		t.Errorf("Body = %q, BodySize = %d, want %q and %d", got[0].Body, got[0].BodySize, body, len(body))
	}
}

func TestConcurrentReadWhileWrite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	done := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer close(done)
		for range 200 {
			if _, err := s.Insert(ctx, &model.Request{Endpoint: "demo", Method: "POST", Path: "/"}); err != nil {
				t.Errorf("Insert: %v", err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, err := s.List(ctx, ListOptions{}); err != nil {
				t.Errorf("List: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}

func TestSetForwardResult(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	target := insertRequest(t, s, "demo", nil)
	other := insertRequest(t, s, "demo", nil)

	if err := s.SetForwardResult(ctx, target, 500, 38, "boom"); err != nil {
		t.Fatalf("SetForwardResult: %v", err)
	}

	got, err := s.Get(ctx, target)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ForwardStatus != 500 || got.ForwardMs != 38 || got.ForwardError != "boom" {
		t.Errorf("got status %d, ms %d, error %q", got.ForwardStatus, got.ForwardMs, got.ForwardError)
	}

	untouched, err := s.Get(ctx, other)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if untouched.ForwardStatus != 0 || untouched.ForwardMs != 0 || untouched.ForwardError != "" {
		t.Errorf("other row changed: %+v", untouched)
	}
}

func TestSetForwardResultNotFound(t *testing.T) {
	s := newTestStore(t)

	err := s.SetForwardResult(context.Background(), 99, 200, 1, "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDeleteOlderThan(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, createdAt := range []int64{100, 200, 300} {
		r := &model.Request{Endpoint: "demo", Method: "POST", Path: "/", CreatedAt: createdAt}
		if _, err := s.Insert(ctx, r); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	// The cutoff is exclusive: the row at exactly 200 stays.
	deleted, err := s.DeleteOlderThan(ctx, 200)
	if err != nil {
		t.Fatalf("DeleteOlderThan: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}

	got, err := s.List(ctx, ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var kept []int64
	for _, r := range got {
		kept = append(kept, r.CreatedAt)
	}
	if want := []int64{300, 200}; !reflect.DeepEqual(kept, want) {
		t.Errorf("kept = %v, want %v", kept, want)
	}
}
