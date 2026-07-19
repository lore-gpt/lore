package core

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/workmem"
)

// Worker is the job-processing composition: the store plus a queue client that
// can work jobs (the Server's cannot). It shares Config and Options with the
// Server, so a downstream build injects the same extension implementations into
// both roles.
type Worker struct {
	store   *store.Store
	queue   *queue.Queue
	ext     extensions
	workmem workmem.Store
}

// NewWorker composes the job worker from cfg. Phase 1 wires the extractor's
// dependencies — using the extension points — here, without touching the Server.
func NewWorker(ctx context.Context, cfg Config, opts ...Option) (*Worker, error) {
	e, err := resolveExtensions(opts)
	if err != nil {
		return nil, err
	}

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	q, err := queue.NewWorker(st, e.extractor, e.adjudicator, e.embedder, e.workmem, e.metrics, e.tracer)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("build worker queue: %w", err)
	}

	return &Worker{store: st, queue: q, ext: e, workmem: e.workmem}, nil
}

// Start begins working jobs until ctx is canceled, then stops gracefully.
//
// River treats cancellation of the context passed to its Start as a hard stop,
// so the queue runs on a non-cancelable context and shutdown goes solely through
// Stop — draining in-flight jobs within a bounded grace period.
func (w *Worker) Start(ctx context.Context) error {
	w.ext.logComposed(ctx, "worker")

	if err := w.queue.Start(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}

	// Reconcile vector indexes once at startup: enqueue a build for any project that pinned a model but has
	// no valid index (a build never enqueued, or lost to a crash), so none is left permanently on the slower
	// exact-scan path. Runs in the background so a large sweep never delays shutdown; it is best-effort and
	// idempotent, so a shutdown that interrupts it (ctx) is simply reconciled on the next start.
	go func() {
		if n, err := w.queue.BackfillMissingIndexes(ctx); err != nil {
			slog.ErrorContext(ctx, "index backfill sweep failed at startup", slog.Any("err", err))
		} else if n > 0 {
			slog.InfoContext(ctx, "index backfill sweep enqueued builds", slog.Int("count", n))
		}
	}()

	// Scrape queue depth and oldest-available-job age periodically (River exposes no Go API for aggregate
	// queue state). Best-effort, stops with ctx; the metrics registry is the no-op default when telemetry is
	// off, so this is a cheap query loop that exports nothing in that case.
	w.ext.metrics.WorkmemMode.Set(workmemModeValue(w.workmem.Mode()))
	go w.queue.CollectStats(ctx, w.ext.metrics, 15*time.Second)

	<-ctx.Done()

	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return w.queue.Stop(stopCtx)
}

// Close releases the Worker's resources (the database pool and, when a downstream
// build injects one via WithWorkmem, the working-memory store's client and probe).
// The OSS worker's default is the disabled no-op, whose Close is a no-op.
func (w *Worker) Close() {
	w.store.Close()
	w.workmem.Close()
}
