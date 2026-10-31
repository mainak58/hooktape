package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mainak58/hooktape/internal/model"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct {
	db   *sql.DB
	path string // kept only so lock errors can name the file in use
}

// Open creates the database if it is missing. The file holds real secrets, so
// it starts owner-only and SQLite gives -wal and -shm the same mode.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	file.Close()

	return open(path)
}

// OpenExisting is Open for commands that only make sense on a database that is
// already there, so a typo'd --db fails instead of looking empty.
func OpenExisting(path string) (*Store, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && info.Size() == 0) {
		return nil, fmt.Errorf("%w at %s", ErrNoDatabase, path)
	}

	return open(path)
}

func open(path string) (*Store, error) {
	if info, err := os.Stat(path); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a database file", path)
	}

	// _txlock makes migrate take the write lock up front, where busy_timeout
	// applies. A lock upgrade halfway through a transaction fails immediately.
	// The timeout outlasts a VACUUM of a few GB, so a delete never costs a webhook.
	dsn := "file:" + uriPath.Replace(path) + "?_pragma=busy_timeout(25000)&_pragma=journal_mode(WAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	s := &Store{db: db, path: path}

	// The first switch to WAL ignores busy_timeout, so processes racing to open
	// a new file have to retry it by hand.
	err = s.migrate()
	for attempt := 0; attempt < 50 && locked(err); attempt++ {
		time.Sleep(100 * time.Millisecond)
		err = s.migrate()
	}
	if err != nil {
		db.Close()
		return nil, s.wrap(err)
	}

	// Files from older versions were world-readable. This comes after the open so
	// a mistyped --db never changes the mode of something that is not a database.
	// Best effort: a database someone else owns can still be read.
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Chmod(file, 0o600)
	}

	return s, nil
}

// uriPath escapes what a SQLite file: URI would otherwise read as its query,
// fragment, or an escape.
var uriPath = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// addedColumns are the columns added since schema.sql. New and old databases
// take the same path, so the upgrade is exercised on every fresh file.
var addedColumns = []string{
	`truncated INTEGER NOT NULL DEFAULT 0`,
	`replay_status INTEGER NOT NULL DEFAULT 0`,
	`replay_ms INTEGER NOT NULL DEFAULT 0`,
	`replay_error TEXT NOT NULL DEFAULT ''`,
	`headers_b64 INTEGER NOT NULL DEFAULT 0`,
}

const schemaVersion = 1

func userVersion(q interface {
	QueryRow(query string, args ...any) *sql.Row
}) (int, error) {
	var version int
	err := q.QueryRow(`PRAGMA user_version`).Scan(&version)

	return version, err
}

// migrate never rewrites a row. It runs in one transaction, so a crash leaves
// the old schema whole, and the version is read again under the write lock
// because another process may have upgraded the file while this one waited.
func (s *Store) migrate() error {
	version, err := userVersion(s.db)
	if err != nil || version >= schemaVersion {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	version, err = userVersion(tx)
	if err != nil || version >= schemaVersion {
		return err
	}

	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	for _, column := range addedColumns {
		if _, err := tx.Exec(`ALTER TABLE requests ADD COLUMN ` + column); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) wrap(err error) error {
	if err == nil {
		return nil
	}

	if locked(err) {
		return fmt.Errorf("another hooktape may be writing to %s; wait or use --db", s.path)
	}

	return err
}

func locked(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()

	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Insert(ctx context.Context, r *model.Request) (int64, error) {
	headers, err := encodeHeaders(r.Headers)
	if err != nil {
		return 0, err
	}

	body := r.Body
	if body == nil {
		body = []byte{}
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO requests (endpoint, method, path, query, headers, headers_b64, body, remote_ip, created_at, truncated)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		r.Endpoint, r.Method, r.Path, r.Query, string(headers), body, r.RemoteIP, r.CreatedAt, r.Truncated)
	if err != nil {
		return 0, s.wrap(err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, s.wrap(err)
	}

	r.ID = id

	return id, nil

}

// encodeHeaders stores values as []byte, which JSON carries as base64. Header
// values are byte strings, and a Go string in JSON turns invalid UTF-8 into
// U+FFFD, which would break a signature on replay.
func encodeHeaders(headers map[string][]string) ([]byte, error) {
	raw := make(map[string][][]byte, len(headers))
	for name, values := range headers {
		raw[name] = make([][]byte, len(values))
		for i, value := range values {
			raw[name][i] = []byte(value)
		}
	}

	return json.Marshal(raw)
}

func decodeHeaders(data []byte) (map[string][]string, error) {
	var raw map[string][][]byte
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	headers := make(map[string][]string, len(raw))
	for name, values := range raw {
		headers[name] = make([]string, len(values))
		for i, value := range values {
			headers[name][i] = string(value)
		}
	}

	return headers, nil
}

const requestColumns = `id, endpoint, method, path, query, headers, headers_b64, remote_ip, created_at,
	forward_status, forward_error, forward_ms, replay_status, replay_error, replay_ms, truncated, length(body)`

func scanRequest(scan func(dest ...any) error, withBody bool) (model.Request, error) {
	var r model.Request
	var headers string
	var headersB64 bool
	dest := []any{
		&r.ID, &r.Endpoint, &r.Method, &r.Path, &r.Query, &headers, &headersB64, &r.RemoteIP, &r.CreatedAt,
		&r.ForwardStatus, &r.ForwardError, &r.ForwardMs, &r.ReplayStatus, &r.ReplayError, &r.ReplayMs, &r.Truncated, &r.BodySize,
	}
	if withBody {
		dest = append(dest, &r.Body)
	}

	if err := scan(dest...); err != nil {
		return model.Request{}, err
	}

	var err error
	if headersB64 {
		r.Headers, err = decodeHeaders([]byte(headers))
	} else {
		err = json.Unmarshal([]byte(headers), &r.Headers)
	}
	if err != nil {
		return model.Request{}, err
	}

	return r, nil
}

func (s *Store) Get(ctx context.Context, id int64) (*model.Request, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+requestColumns+`, body FROM requests WHERE id = ?`, id)

	r, err := scanRequest(row.Scan, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, s.wrap(err)
	}

	return &r, nil
}

func (s *Store) List(ctx context.Context, opts ListOptions) ([]model.Request, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	query := `SELECT ` + requestColumns
	if opts.WithBody {
		query += `, body`
	}
	query += ` FROM requests`

	var args []any
	if opts.Endpoint != "" {
		query += ` WHERE endpoint = ?`
		args = append(args, opts.Endpoint)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, s.wrap(err)
	}
	defer rows.Close()

	requests := []model.Request{}
	for rows.Next() {
		r, err := scanRequest(rows.Scan, opts.WithBody)
		if err != nil {
			return nil, s.wrap(err)
		}
		requests = append(requests, r)
	}

	return requests, s.wrap(rows.Err())
}

// SetForwardResult records the live delivery. It stays as it was, whatever a
// later replay does.
func (s *Store) SetForwardResult(ctx context.Context, id int64, status int, ms int64, errMsg string) error {
	return s.setResult(ctx, id, `UPDATE requests SET forward_status = ?, forward_ms = ?, forward_error = ? WHERE id = ?`, status, ms, errMsg)
}

// SetReplayResult records the latest replay, replacing the one before it.
func (s *Store) SetReplayResult(ctx context.Context, id int64, status int, ms int64, errMsg string) error {
	return s.setResult(ctx, id, `UPDATE requests SET replay_status = ?, replay_ms = ?, replay_error = ? WHERE id = ?`, status, ms, errMsg)
}

func (s *Store) setResult(ctx context.Context, id int64, update string, status int, ms int64, errMsg string) error {
	res, err := s.db.ExecContext(ctx, update, status, ms, errMsg, id)
	if err != nil {
		return s.wrap(err)
	}

	updated, err := res.RowsAffected()
	if err != nil {
		return s.wrap(err)
	}
	if updated == 0 {
		return ErrNotFound
	}

	return nil
}

// DeleteFilter selects the half-open range [FromMs, ToMs) of created_at. Zero
// means unbounded, so the zero filter matches every row.
type DeleteFilter struct {
	FromMs, ToMs int64
	Endpoint     string // "" = all
}

func (f DeleteFilter) where() (string, []any) {
	var clauses []string
	var args []any

	if f.FromMs != 0 {
		clauses = append(clauses, `created_at >= ?`)
		args = append(args, f.FromMs)
	}
	if f.ToMs != 0 {
		clauses = append(clauses, `created_at < ?`)
		args = append(args, f.ToMs)
	}
	if f.Endpoint != "" {
		clauses = append(clauses, `endpoint = ?`)
		args = append(args, f.Endpoint)
	}
	if len(clauses) == 0 {
		return "", nil
	}

	return ` WHERE ` + strings.Join(clauses, ` AND `), args
}

func (s *Store) CountMatching(ctx context.Context, f DeleteFilter) (int64, error) {
	where, args := f.where()

	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests`+where, args...).Scan(&count)

	return count, s.wrap(err)
}

func (s *Store) DeleteMatching(ctx context.Context, f DeleteFilter) (int64, error) {
	where, args := f.where()

	res, err := s.db.ExecContext(ctx, `DELETE FROM requests`+where, args...)
	if err != nil {
		return 0, s.wrap(err)
	}

	deleted, err := res.RowsAffected()

	return deleted, s.wrap(err)
}

type ListOptions struct {
	Endpoint string // "" = all
	Limit    int    // <=0 means 50
	WithBody bool   // false = don't load body column (fast list)
}

var ErrNotFound = errors.New("request not found")

// ErrNoDatabase is what OpenExisting returns when the file is not there.
var ErrNoDatabase = errors.New("no database")

// maxIDsPerDelete stays well under SQLite's 32,766 parameter limit.
const maxIDsPerDelete = 500

func (s *Store) DeleteByIDs(ctx context.Context, ids []int64) (int64, error) {
	var total int64

	for chunk := range slices.Chunk(ids, maxIDsPerDelete) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}

		res, err := s.db.ExecContext(ctx,
			`DELETE FROM requests WHERE id IN (?`+strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return total, s.wrap(err)
		}

		deleted, err := res.RowsAffected()
		if err != nil {
			return total, s.wrap(err)
		}
		total += deleted
	}

	return total, nil
}

// Vacuum returns disk space and scrubs deleted rows from the file. The
// checkpoint matters: VACUUM leaves the old pages behind in the WAL until then.
func (s *Store) Vacuum(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return s.wrap(err)
	}

	// A checkpoint blocked by a reader is a row of results, not an error.
	var busy, frames, checkpointed int
	err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed)
	if err != nil {
		return s.wrap(err)
	}
	if busy != 0 {
		return errors.New("another process is reading the database, so deleted data stays in the -wal file until it stops; run this again then")
	}

	return nil
}
