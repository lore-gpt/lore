// Package queue wires the River job queue over the shared pgx pool.
//
// There are two client shapes on purpose (ADR-014 composition: each binary
// wires only what it needs):
//
//   - New       — insert-only. `lore serve` enqueues jobs (InsertTx) but has no
//     queues/workers configured, so it *structurally* cannot process jobs. A
//     stray Start() is a returned error, not a silent role change.
//   - NewWorker — full worker. `lore worker` processes jobs; Phase 1 wires the
//     extractor's dependencies here without touching the server.
package queue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/lore-gpt/lore/core/jobs"
)

// Queue owns a River client and the pool it runs on. Whether it can work jobs
// is fixed at construction (New vs NewWorker), not by convention.
type Queue struct {
	Client *river.Client[pgx.Tx]
	pool   *pgxpool.Pool
	worker bool
}

// New builds an insert-only River client for the server. It can enqueue via
// InsertTx but has no queues or workers, so Start is rejected.
func New(pool *pgxpool.Pool) (*Queue, error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		return nil, fmt.Errorf("create river client: %w", err)
	}
	return &Queue{Client: client, pool: pool, worker: false}, nil
}

// NewWorker builds a River client that processes extract_event jobs on the
// default queue. `lore worker` uses this and calls Start.
func NewWorker(pool *pgxpool.Pool) (*Queue, error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, jobs.NewExtractEventWorker())

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
		},
		Workers: workers,
	})
	if err != nil {
		return nil, fmt.Errorf("create river worker client: %w", err)
	}
	return &Queue{Client: client, pool: pool, worker: true}, nil
}

// Start begins working jobs. It errors on an insert-only client (built with
// New) so the server can never silently become a worker.
func (q *Queue) Start(ctx context.Context) error {
	if !q.worker {
		return errors.New("queue: insert-only client, use NewWorker")
	}
	return q.Client.Start(ctx)
}

// Stop gracefully drains and stops the worker.
func (q *Queue) Stop(ctx context.Context) error {
	return q.Client.Stop(ctx)
}

// Ping reports queue health for /healthz: the River schema must be migrated and
// reachable. Available on both client shapes (the server needs it).
func (q *Queue) Ping(ctx context.Context) error {
	var reg *string
	if err := q.pool.QueryRow(ctx, "SELECT to_regclass('river_job')::text").Scan(&reg); err != nil {
		return fmt.Errorf("check river schema: %w", err)
	}
	if reg == nil {
		return errors.New("river schema not migrated")
	}
	return nil
}
