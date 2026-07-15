package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/lore-gpt/lore/core/ext"
)

// IndexQueue is the low-priority River queue the vector-index builds run on, kept off the default queue so
// a slow CREATE INDEX CONCURRENTLY never starves the extraction workers.
const IndexQueue = "index"

// EnsureIndexArgs enqueues a one-time vector-index build for a project's embedding partition. It is inserted
// when a project first pins its embedding model (atomically with the pin) and by a startup sweep for any
// pinned project whose index is missing. It is unique per project, so concurrent enqueues coalesce into one
// build.
type EnsureIndexArgs struct {
	ProjectID string `json:"project_id"`
}

// Kind is the stable job identifier River persists.
func (EnsureIndexArgs) Kind() string { return "ensure_index" }

// InsertOpts runs the build on the low-priority index queue, unique per project (concurrent enqueues
// coalesce into one), with retries so a transient failure — or an interrupted CONCURRENTLY build that left
// an INVALID index — is retried and self-healed by EnsureIndex on the next attempt. Completed is excluded
// from the unique states so a later genuine rebuild (e.g. after an index is dropped) can be re-enqueued.
func (EnsureIndexArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       IndexQueue,
		MaxAttempts: 5,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
				rivertype.JobStateRetryable,
			},
		},
	}
}

// VectorIndexer builds the vector index on a project's embedding partition. *store.PgVectorIndex satisfies
// it; naming just the one method keeps jobs off the whole store surface and lets a test supply a fake.
type VectorIndexer interface {
	EnsureIndex(ctx context.Context, projectID pgtype.UUID, dim int) error
}

// IndexEnqueuer enqueues a one-time index build for a project on the caller's transaction, so the enqueue
// commits atomically with the model pin (and never fires for a rolled-back pass). A nil enqueuer — the
// persister's default — skips it: the model is still pinned, and the worker's startup sweep reconciles any
// project left without an index. The concrete implementation lives in the queue package (over the River
// client), keeping jobs free of a queue dependency.
type IndexEnqueuer interface {
	EnqueueEnsureIndex(ctx context.Context, tx pgx.Tx, projectID pgtype.UUID) error
}

// EnsureIndexWorker builds the per-partition vector index off the write path. The dimension comes from the
// composed embedder — the same single source of truth the pin derives from — so the index is built for the
// model the project adopted. The build is idempotent and self-healing (see store.PgVectorIndex.EnsureIndex),
// so a retry after a crash or an interrupted build converges.
type EnsureIndexWorker struct {
	river.WorkerDefaults[EnsureIndexArgs]
	index    VectorIndexer
	embedder ext.Embedder
}

// NewEnsureIndexWorker builds the index-build worker over the vector index and the composed embedder.
func NewEnsureIndexWorker(index VectorIndexer, embedder ext.Embedder) *EnsureIndexWorker {
	return &EnsureIndexWorker{index: index, embedder: embedder}
}

// Timeout removes the per-job deadline. A CREATE INDEX CONCURRENTLY on a large partition can run far longer
// than River's default one-minute job timeout, and a build cancelled by that timeout leaves an INVALID index
// that the next attempt would only re-cancel — so the default would prevent the index from ever building for
// exactly the partitions large enough to need it. -1 means the job's context is not deadline-bounded (it is
// still cancelled on worker shutdown). The build is isolated on its own single-worker queue, so an unbounded
// build never starves extraction.
func (w *EnsureIndexWorker) Timeout(*river.Job[EnsureIndexArgs]) time.Duration { return -1 }

// Work builds (or re-heals) the project's vector index. On failure it returns an error so River retries;
// the index is a performance optimisation — recall works on the exact-scan path without it — so a build that
// exhausts its attempts is logged loudly at the final attempt but does not otherwise affect correctness.
func (w *EnsureIndexWorker) Work(ctx context.Context, job *river.Job[EnsureIndexArgs]) error {
	projectID, err := parseUUID(job.Args.ProjectID)
	if err != nil {
		return fmt.Errorf("ensure_index: parse project id %q: %w", job.Args.ProjectID, err)
	}
	if err := w.index.EnsureIndex(ctx, projectID, w.embedder.Dim()); err != nil {
		if job.Attempt >= job.MaxAttempts {
			slog.ErrorContext(ctx, "ensure_index: vector index build failed after the final attempt; recall stays on the exact-scan path for this project",
				slog.String("project_id", job.Args.ProjectID), slog.Int("attempt", job.Attempt), slog.Any("err", err))
		}
		return fmt.Errorf("ensure index for project %s: %w", job.Args.ProjectID, err)
	}
	slog.InfoContext(ctx, "ensure_index: vector index ready",
		slog.String("project_id", job.Args.ProjectID), slog.Int("dim", w.embedder.Dim()))
	return nil
}
