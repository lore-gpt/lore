// Package store is the persistence layer: a pgx connection pool plus the
// sqlc-generated queries (see the db subpackage) and the embedded migrations.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// Store owns the pgx connection pool shared by the server and worker.
type Store struct {
	Pool *pgxpool.Pool
}

// New opens a pgx pool for dsn and verifies connectivity before returning.
//
// Every connection registers the pgvector types, so `vector` columns encode and
// decode natively: binary format, and — crucially — NULL-safe, which pgvector.Vector's
// own sql.Scanner cannot manage on its own (it errors on a NULL). Registration needs
// the `vector` extension, which the migrations install, so New must run after
// migrations — which it does in every boot path (`lore migrate` and `serve
// --auto-migrate` both apply migrations first).
//
// The caller owns the returned Store and must call Close.
func New(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse pool config: %w", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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

// WithProject runs fn inside a transaction whose lore.project_id session setting is set to
// projectID, so the Row-Level Security policies (migration 0008) scope every statement fn issues
// to that one project. It uses set_config with is_local => true, which is transaction-scoped, so
// the setting is discarded at commit/rollback and never leaks to the next user of a pooled
// connection. fn must run its queries on the provided tx.
//
// An error from fn rolls the transaction back. An unset project id is refused, so a tenant query
// can never run with an empty scope. (RLS would treat an empty scope as "see nothing" regardless,
// but failing here turns a silent zero-row result into an explicit error.)
func (s *Store) WithProject(ctx context.Context, projectID pgtype.UUID, fn func(pgx.Tx) error) error {
	pid, err := projectIDText(projectID)
	if err != nil {
		return err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tenant transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('lore.project_id', $1, true)`, pid); err != nil {
		return fmt.Errorf("set tenant scope: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
