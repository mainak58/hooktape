package store

import (
	"context"
	"errors"

	"github.com/mainak58/hooktape/internal/model"
)

type Store struct { /* *sql.DB */
}

func Open(path string) (*Store, error)
func (s *Store) Close() error
func (s *Store) Insert(ctx context.Context, r *model.Request) (int64, error) // sets r.ID
func (s *Store) Get(ctx context.Context, id int64) (*model.Request, error)   // ErrNotFound if missing
func (s *Store) List(ctx context.Context, opts ListOptions) ([]model.Request, error)
func (s *Store) SetForwardResult(ctx context.Context, id int64, status int, ms int64, errMsg string) error
func (s *Store) DeleteOlderThan(ctx context.Context, cutoffMs int64) (int64, error)

type ListOptions struct {
	Endpoint string // "" = all
	Limit    int    // <=0 means 50
	WithBody bool   // false = don't load body column (fast list)
}

var ErrNotFound = errors.New("request not found")
