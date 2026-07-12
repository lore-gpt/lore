package core

import (
	"context"
	"fmt"
	"time"

	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
)

// Worker is the job-processing composition: the store plus a queue client that
// can work jobs (the Server's cannot). It shares Config and Options with the
// Server, so a downstream build injects the same extension implementations into
// both roles.
type Worker struct {
	store *store.Store
	queue *queue.Queue
	ext   extensions
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
	q, err := queue.NewWorker(st.Pool, e.extractor)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("build worker queue: %w", err)
	}

	return &Worker{store: st, queue: q, ext: e}, nil
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
	<-ctx.Done()

	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return w.queue.Stop(stopCtx)
}

// Close releases the Worker's resources (the database pool).
func (w *Worker) Close() {
	w.store.Close()
}
