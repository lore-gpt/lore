// Package queue wires the River job queue over the shared pgx pool.
//
// There are two client shapes on purpose (open-core composition: each binary
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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
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

// NewWorker builds a River client that processes extract_run jobs on the default
// queue: it reads events through the pool, distils them with the given Extractor,
// persists the result (advancing the run checkpoint) through the store's
// tenant-scoped transactions, resolves claim conflicts with the given Adjudicator,
// and embeds stored memories with the given Embedder. The working-memory store
// routes kind:"state" events (hot lane when healthy, a durable claim otherwise).
// `lore worker` uses this and calls Start.
func NewWorker(st *store.Store, extractor ext.Extractor, adjudicator ext.Adjudicator, embedder ext.Embedder, wm workmem.Store) (*Queue, error) {
	pool := st.Pool
	workers := river.NewWorkers()
	// The worker reads events straight through db.New(pool) but writes through the store's
	// tenant-scoped transactions (NewPGPersister sets lore.project_id). Both are correct today because
	// the worker connects as the RLS-bypassing pool owner. When that role is cut over to the
	// RLS-subject application role, these reads must also set the per-run project scope (via the
	// store), or the tenant policies would return no rows and extraction would silently stall — the
	// writes are already scoped, the reads are not yet.
	river.AddWorker(workers, jobs.NewExtractRunWorker(
		db.New(pool), extractor, jobs.NewPGPersister(st, adjudicator, embedder, jobs.WithIndexEnqueuer(indexEnqueuer{})), jobs.DefaultDebounce(),
		jobs.WithWorkmemStore(wm)))
	// The vector-index build runs off the write path: the persister enqueues it when a project first pins its
	// model, and it builds the per-partition HNSW with the composed embedder's dimension.
	river.AddWorker(workers, jobs.NewEnsureIndexWorker(store.NewPgVectorIndex(pool), embedder))

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
			// The index queue runs one slow CREATE INDEX CONCURRENTLY at a time, off the default queue, so it
			// never starves the extraction workers.
			jobs.IndexQueue: {MaxWorkers: 1},
		},
		Workers: workers,
	})
	if err != nil {
		return nil, fmt.Errorf("create river worker client: %w", err)
	}
	return &Queue{Client: client, pool: pool, worker: true}, nil
}

// indexEnqueuer enqueues an ensure_index job on the caller's transaction using the River client bound to the
// worker context. A persister call always runs inside a worker's Work, so the client is present; outside a
// worker (a direct Persist in a test) there is no client in context and it is a no-op — the model is still
// pinned, and the startup sweep would reconcile the index.
type indexEnqueuer struct{}

func (indexEnqueuer) EnqueueEnsureIndex(ctx context.Context, tx pgx.Tx, projectID pgtype.UUID) error {
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return nil // not running under a worker (e.g. a direct persister test): nothing to enqueue
	}
	if _, err := client.InsertTx(ctx, tx, jobs.EnsureIndexArgs{ProjectID: uuid.UUID(projectID.Bytes).String()}, nil); err != nil {
		return fmt.Errorf("enqueue ensure_index: %w", err)
	}
	return nil
}

// BackfillMissingIndexes enqueues an ensure_index job for every project that has pinned an embedding model
// but whose partition carries no valid vector index — closing the gap between a pin and its enqueue, and
// re-driving any build lost to a crash, so no project is left permanently on the exact-scan path. It runs
// once at worker startup. Idempotent: the jobs are unique per project and EnsureIndex is a no-op when the
// index already exists. Returns how many builds it enqueued.
func (q *Queue) BackfillMissingIndexes(ctx context.Context) (int, error) {
	ids, err := db.New(q.pool).ListProjectsWithActiveModel(ctx)
	if err != nil {
		return 0, fmt.Errorf("list pinned projects: %w", err)
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin index sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var missing []pgtype.UUID
	for _, id := range ids {
		ok, err := store.HasValidEmbeddingIndex(ctx, tx, id)
		if err != nil {
			return 0, fmt.Errorf("check index for %s: %w", uuid.UUID(id.Bytes), err)
		}
		if !ok {
			missing = append(missing, id)
		}
	}
	_ = tx.Rollback(ctx)

	for _, id := range missing {
		if _, err := q.Client.Insert(ctx, jobs.EnsureIndexArgs{ProjectID: uuid.UUID(id.Bytes).String()}, nil); err != nil {
			return 0, fmt.Errorf("enqueue ensure_index for %s: %w", uuid.UUID(id.Bytes), err)
		}
	}
	return len(missing), nil
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

// EnqueueExtract inserts a coalesced extract_run job for the event's run on the
// given transaction, so the enqueue commits atomically with the event insert. The
// job is unique per run, so a burst of events for one run collapses into a single
// extraction pass. Available on both client shapes: the insert-only server
// enqueues here; only working the job requires NewWorker.
func (q *Queue) EnqueueExtract(ctx context.Context, tx pgx.Tx, projectID, runID string) error {
	if _, err := q.Client.InsertTx(ctx, tx, jobs.ExtractRunArgs{ProjectID: projectID, RunID: runID}, nil); err != nil {
		return fmt.Errorf("enqueue extract_run: %w", err)
	}
	return nil
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
