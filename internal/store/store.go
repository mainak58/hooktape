package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mainak58/hooktape/internal/model"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct {
	db   *sql.DB
	path string // kept only so lock errors can name the file in use
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	s := &Store{db: db, path: path}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, s.wrap(err)
	}

	return s, nil
}

func (s *Store) wrap(err error) error {
	if err == nil {
		return nil
	}

	msg := err.Error()
	if strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") {
		return fmt.Errorf("another hooktape may be writing to %s; wait or use --db", s.path)
	}

	return err
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Insert(ctx context.Context, r *model.Request) (int64, error) {
	headers, err := json.Marshal(r.Headers)
	if err != nil {
		return 0, err
	}

	body := r.Body
	if body == nil {
		body = []byte{}
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO requests (endpoint, method, path, query, headers, body, remote_ip, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Endpoint, r.Method, r.Path, r.Query, string(headers), body, r.RemoteIP, r.CreatedAt)
	if err != nil {
		return 0, s.wrap(err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	r.ID = id

	return id, nil

}

const requestColumns = `id, endpoint, method, path, query, headers, remote_ip, created_at,
	forward_status, forward_error, forward_ms, length(body)`

func scanRequest(scan func(dest ...any) error, withBody bool) (model.Request, error) {
	var r model.Request
	var headers string
	dest := []any{
		&r.ID, &r.Endpoint, &r.Method, &r.Path, &r.Query, &headers, &r.RemoteIP, &r.CreatedAt,
		&r.ForwardStatus, &r.ForwardError, &r.ForwardMs, &r.BodySize,
	}
	if withBody {
		dest = append(dest, &r.Body)
	}

	if err := scan(dest...); err != nil {
		return model.Request{}, err
	}
	if err := json.Unmarshal([]byte(headers), &r.Headers); err != nil {
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
			return nil, err
		}
		requests = append(requests, r)
	}

	return requests, s.wrap(rows.Err())
}

func (s *Store) SetForwardResult(ctx context.Context, id int64, status int, ms int64, errMsg string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE requests SET forward_status = ?, forward_ms = ?, forward_error = ?
		WHERE id = ?`,
		status, ms, errMsg, id)
	if err != nil {
		return s.wrap(err)
	}

	updated, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return ErrNotFound
	}

	return nil
}

func (s *Store) DeleteOlderThan(ctx context.Context, cutoffMs int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM requests WHERE created_at < ?`, cutoffMs)
	if err != nil {
		return 0, s.wrap(err)
	}

	return res.RowsAffected()
}

type ListOptions struct {
	Endpoint string // "" = all
	Limit    int    // <=0 means 50
	WithBody bool   // false = don't load body column (fast list)
}

var ErrNotFound = errors.New("request not found")

func (s *Store) DeleteByIDs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	res, err := s.db.ExecContext(ctx,
		`DELETE FROM requests WHERE id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, args...)
	if err != nil {
		return 0, s.wrap(err)
	}

	return res.RowsAffected()
}

func (s *Store) DeleteAll(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM requests`)
	if err != nil {
		return 0, s.wrap(err)
	}

	return res.RowsAffected()
}
