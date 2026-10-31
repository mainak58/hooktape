package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		if busyTimeout != 25000 {
			t.Errorf("busy_timeout = %d, want 25000", busyTimeout)
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

func TestSetReplayResultLeavesLiveDelivery(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := insertRequest(t, s, "demo", nil)

	if err := s.SetForwardResult(ctx, id, 200, 41, ""); err != nil {
		t.Fatalf("SetForwardResult: %v", err)
	}
	if err := s.SetReplayResult(ctx, id, 0, 3, "connection refused"); err != nil {
		t.Fatalf("SetReplayResult: %v", err)
	}

	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ForwardStatus != 200 || got.ForwardMs != 41 || got.ForwardError != "" {
		t.Errorf("replay changed the live delivery: %+v", got)
	}
	if got.ReplayStatus != 0 || got.ReplayMs != 3 || got.ReplayError != "connection refused" {
		t.Errorf("replay = %d, %d, %q", got.ReplayStatus, got.ReplayMs, got.ReplayError)
	}

	if err := s.SetReplayResult(ctx, 99, 200, 1, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestHeaderAndBodyFidelity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	want := &model.Request{
		Endpoint: "demo",
		Method:   "POST",
		Path:     "/",
		Headers: map[string][]string{
			"X-Sig":   {"A\xffB\xc3C"},
			"X-Multi": {"one", "", "caf\u00e9"},
		},
		Body:      []byte{'A', 0x00, 'B', 0xff, 0xfe, 'C'},
		Truncated: true,
	}
	id, err := s.Insert(ctx, want)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got.Headers, want.Headers) {
		t.Errorf("headers = %q, want %q", got.Headers, want.Headers)
	}
	if !bytes.Equal(got.Body, want.Body) {
		t.Errorf("body = %x, want %x", got.Body, want.Body)
	}
	if !got.Truncated {
		t.Error("Truncated was lost")
	}
}

func TestDeleteMatching(t *testing.T) {
	tests := []struct {
		name   string
		filter DeleteFilter
		kept   []int64
	}{
		{"to is exclusive", DeleteFilter{ToMs: 200}, []int64{200, 300, 300}},
		{"from is inclusive", DeleteFilter{FromMs: 200}, []int64{100}},
		{"both bounds", DeleteFilter{FromMs: 200, ToMs: 300}, []int64{100, 300, 300}},
		{"endpoint narrows the range", DeleteFilter{FromMs: 300, Endpoint: "other"}, []int64{100, 200, 300}},
		{"endpoint alone", DeleteFilter{Endpoint: "other"}, []int64{100, 200, 300}},
		{"zero filter matches all", DeleteFilter{}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			for i, createdAt := range []int64{100, 200, 300, 300} {
				endpoint := "demo"
				if i == 3 {
					endpoint = "other"
				}
				r := &model.Request{Endpoint: endpoint, Method: "POST", Path: "/", CreatedAt: createdAt}
				if _, err := s.Insert(ctx, r); err != nil {
					t.Fatalf("Insert: %v", err)
				}
			}

			wantDeleted := int64(4 - len(tt.kept))
			count, err := s.CountMatching(ctx, tt.filter)
			if err != nil {
				t.Fatalf("CountMatching: %v", err)
			}
			if count != wantDeleted {
				t.Errorf("count = %d, want %d", count, wantDeleted)
			}

			deleted, err := s.DeleteMatching(ctx, tt.filter)
			if err != nil {
				t.Fatalf("DeleteMatching: %v", err)
			}
			if deleted != wantDeleted {
				t.Errorf("deleted = %d, want %d", deleted, wantDeleted)
			}

			got, err := s.List(ctx, ListOptions{})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			var kept []int64
			for _, r := range got {
				kept = slices.Insert(kept, 0, r.CreatedAt)
			}
			if !slices.Equal(kept, tt.kept) {
				t.Errorf("kept = %v, want %v", kept, tt.kept)
			}
		})
	}
}

func TestDeleteByIDsPastTheVariableLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first := insertRequest(t, s, "demo", nil)
	last := insertRequest(t, s, "demo", nil)

	ids := make([]int64, 40000)
	for i := range ids {
		ids[i] = int64(i) + last + 1
	}
	ids[0], ids[len(ids)-1] = first, last

	deleted, err := s.DeleteByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("DeleteByIDs: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}

	if deleted, err := s.DeleteByIDs(ctx, nil); err != nil || deleted != 0 {
		t.Errorf("no ids: deleted = %d, err = %v", deleted, err)
	}
}

func TestVacuumScrubsDeletedRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	secret := []byte("super-secret-token")
	id := insertRequest(t, s, "demo", secret)
	if _, err := s.DeleteByIDs(ctx, []int64{id}); err != nil {
		t.Fatalf("DeleteByIDs: %v", err)
	}
	if err := s.Vacuum(ctx); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}

	for _, file := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(file)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ReadFile: %v", err)
		}
		if bytes.Contains(data, secret) {
			t.Errorf("%s still holds the deleted body", filepath.Base(file))
		}
	}
}

func TestOpenIsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	path := filepath.Join(dir, "test.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertRequest(t, s, "demo", nil)
	defer s.Close()

	for file, want := range map[string]os.FileMode{dir: 0o700, path: 0o600, path + "-wal": 0o600, path + "-shm": 0o600} {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", filepath.Base(file), got, want)
		}
	}

	loose := filepath.Join(t.TempDir(), "loose.db")
	if err := os.WriteFile(loose, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := Open(loose)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer old.Close()
	if info, _ := os.Stat(loose); info.Mode().Perm() != 0o600 {
		t.Errorf("existing db mode = %o, want 600", info.Mode().Perm())
	}
}

func TestOpenExisting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "typo")
	path := filepath.Join(dir, "nope.db")

	if _, err := OpenExisting(path); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("err = %v, want ErrNoDatabase", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenExisting created something: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.Close()

	s, err = OpenExisting(path)
	if err != nil {
		t.Fatalf("OpenExisting: %v", err)
	}
	s.Close()
}

// legacyDB builds a file as the first release wrote it: WAL, schema.sql alone,
// headers in plain JSON, user_version 0, and a forward result on the row.
func legacyDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	_, err = db.Exec(`INSERT INTO requests
		(endpoint, method, path, query, headers, body, remote_ip, created_at, forward_status, forward_ms)
		VALUES ('stripe', 'POST', '/charges', 'a=1', '{"Stripe-Signature":["abcd"]}', x'00ff', '1.2.3.4', 1000, 200, 38)`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	return path
}

func TestLegacyDatabaseUpgradesWithRowsIntact(t *testing.T) {
	path := legacyDB(t)
	ctx := context.Background()

	// Twice: the second open must find nothing left to do.
	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}

		got, err := s.Get(ctx, 1)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		// "abcd" is valid base64, so reading a legacy row as the new format
		// would silently hand back different bytes.
		if want := []string{"abcd"}; !reflect.DeepEqual(got.Headers["Stripe-Signature"], want) {
			t.Errorf("headers = %q, want %q", got.Headers, want)
		}
		if !bytes.Equal(got.Body, []byte{0x00, 0xff}) || got.CreatedAt != 1000 || got.ForwardStatus != 200 || got.ForwardMs != 38 {
			t.Errorf("row changed: %+v", got)
		}
		if got.Truncated || got.ReplayStatus != 0 || got.ReplayError != "" {
			t.Errorf("new columns not zero: %+v", got)
		}

		var version int
		if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			t.Fatalf("user_version: %v", err)
		}
		if version != schemaVersion {
			t.Errorf("user_version = %d, want %d", version, schemaVersion)
		}

		id := insertRequest(t, s, "new", nil)
		if _, err := s.DeleteByIDs(ctx, []int64{id}); err != nil {
			t.Fatalf("DeleteByIDs: %v", err)
		}
		s.Close()
	}
}

func TestConcurrentOpensUpgradeOnce(t *testing.T) {
	path := legacyDB(t)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			s, err := Open(path)
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			defer s.Close()
			if _, err := s.Get(context.Background(), 1); err != nil {
				t.Errorf("Get: %v", err)
			}
		})
	}
	wg.Wait()
}

func TestWrapLockError(t *testing.T) {
	s := &Store{path: "./hooktape.db"}
	locked := "another hooktape may be writing to ./hooktape.db; wait or use --db"

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"busy code", errors.New("SQLITE_BUSY (5): database is busy"), locked},
		{"database locked", errors.New("database is locked (5) (SQLITE_BUSY)"), locked},
		{"table locked", errors.New("database table is locked: requests"), locked},
		{"not found passes through", ErrNotFound, ErrNotFound.Error()},
		{"other passes through", errors.New("no such column: nope"), "no such column: nope"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.wrap(tt.err)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("wrap = %v, want nil", got)
				}
				return
			}
			if got == nil || got.Error() != tt.want {
				t.Fatalf("wrap = %v, want %q", got, tt.want)
			}
		})
	}
}

func TestOpenLeavesNonDatabasesAlone(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "deploy.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho this is well past a sqlite header\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{script, dir} {
		for name, open := range map[string]func(string) (*Store, error){"Open": Open, "OpenExisting": OpenExisting} {
			if s, err := open(path); err == nil {
				s.Close()
				t.Errorf("%s(%s) succeeded", name, path)
			}
		}
	}

	for path, want := range map[string]os.FileMode{script: 0o755, dir: 0o755} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
}

func TestOpenPathWithURICharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a?b#c%d.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	insertRequest(t, s, "stripe", []byte("hello"))

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 && !fileExists(path+"-wal") {
		t.Errorf("the data did not land in %s", path)
	}
	if fileExists(filepath.Join(filepath.Dir(path), "a")) {
		t.Error("the ? was read as a query, so the data went to a file named a")
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestConcurrentOpensOfANewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.db")

	var group sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		group.Go(func() {
			s, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			s.Close()
		})
	}
	group.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("Open: %v", err)
	}
}

func TestVacuumReportsAReaderHoldingTheWAL(t *testing.T) {
	s := newTestStore(t)
	insertRequest(t, s, "stripe", []byte("first"))
	insertRequest(t, s, "stripe", []byte("second"))

	reader, err := OpenExisting(s.path)
	if err != nil {
		t.Fatalf("OpenExisting: %v", err)
	}
	defer reader.Close()

	// _txlock makes every Begin a writer, so an unfinished query is what holds a
	// read snapshot open here.
	rows, err := reader.db.Query(`SELECT id FROM requests`)
	if err != nil {
		t.Fatalf("hold a read: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row to hold")
	}

	insertRequest(t, s, "stripe", []byte("third"))

	// One connection, so the short timeout applies to the checkpoint below.
	s.db.SetMaxOpenConns(1)
	if _, err := s.db.Exec(`PRAGMA busy_timeout = 100`); err != nil {
		t.Fatal(err)
	}

	if err := s.Vacuum(context.Background()); err == nil {
		t.Error("Vacuum reported success with deleted data still in the WAL")
	}
}
