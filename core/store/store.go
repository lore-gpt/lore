// Package store is the persistence layer: a pgx connection pool plus the
// sqlc-generated queries (see the db subpackage) and the embedded migrations.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store owns the pgx connection pool shared by the server and worker.
type Store struct {
	Pool *pgxpool.Pool
}

// New opens a pgx pool for dsn and verifies connectivity before returning.
// The caller owns the returned Store and must call Close.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("create pgx pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Ping checks that the database is reachable. It backs the /healthz db probe.
func (s *Store) Ping(ctx context.Context) error {
	return s.Pool.Ping(ctx)
}

// Close releases the pool's connections.
func (s *Store) Close() {
	s.Pool.Close()
}
