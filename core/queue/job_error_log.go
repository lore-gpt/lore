package queue

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// WorkerOption customises the worker client. Options carry optional dependencies so one can be added
// without changing every construction site.
type WorkerOption func(*workerOptions)

type workerOptions struct {
	logger *slog.Logger
}

// WithJobErrorLogger overrides the logger that failed jobs are reported on. Unset, they go to the
// process default logger — which is where an operator watching the worker is already looking.
func WithJobErrorLogger(l *slog.Logger) WorkerOption {
	return func(o *workerOptions) { o.logger = l }
}

// jobErrorReporter reports a failed job attempt on the worker's own log.
//
// The queue already records every failure durably and reports it through its own logger — but at info
// level, while that logger is built at warn by default. The failure is real, the retry is real, and the
// error text is in the database; the operator is the only one left out. A pass that keeps rolling back
// therefore reads as a silent stall: the last thing on stdout is the start of a pass that never
// finishes, with nothing to say why.
//
// Reporting here puts the failure back in front of whoever is watching, and separates the two cases
// that deserve different reactions: an attempt that will be retried, and the final attempt, after which
// the job is discarded and its work stays undone until something enqueues it again.
//
// Routine snoozes never reach this path. A snooze is delivered as a sentinel error but is settled
// before the error handler runs, so a debounced pass that reschedules itself stays quiet.
type jobErrorReporter struct{ logger *slog.Logger }

// log resolves the logger late so that swapping the process default after construction still takes
// effect, and so a zero value is usable.
func (r *jobErrorReporter) log() *slog.Logger {
	if r.logger == nil {
		return slog.Default()
	}
	return r.logger
}

// HandleError reports a failed attempt. It returns nil so the job keeps the retry schedule the queue
// was configured with: this handler observes, it never changes what happens to the job.
//
// The error text is included deliberately — a report that says a job failed without saying why sends
// the operator to the database, which is the situation this exists to fix. Worker errors are built
// from ids, kinds and counts, with one exception: a failure to store an entity names that entity, and
// entity names come from extracted content. Worker logs should therefore be treated as carrying short
// content-derived identifiers, not as content-free.
func (r *jobErrorReporter) HandleError(ctx context.Context, job *rivertype.JobRow, err error) *river.ErrorHandlerResult {
	attrs := append(jobAttrs(job), slog.String("error", err.Error()))
	if job.Attempt >= job.MaxAttempts {
		r.log().ErrorContext(ctx, "background job failed on its final attempt and will not be retried", attrs...)
	} else {
		r.log().WarnContext(ctx, "background job failed and will be retried", attrs...)
	}
	return nil
}

// HandlePanic reports a panic. A panic is a defect however many attempts remain, so it is always at
// error level and always carries the stack — the trace is the only evidence of where it came from.
// It likewise returns nil, leaving the job's fate to the configured retry policy.
func (r *jobErrorReporter) HandlePanic(ctx context.Context, job *rivertype.JobRow, panicVal any, trace string) *river.ErrorHandlerResult {
	attrs := append(jobAttrs(job),
		slog.String("panic", fmt.Sprintf("%v", panicVal)),
		slog.String("trace", trace))
	r.log().ErrorContext(ctx, "background job panicked", attrs...)
	return nil
}

// jobAttrs is the identity every job report carries: which job, which kind, and how far into its
// retry budget it got. attempt/max_attempts is what tells an operator whether to wait or to act.
func jobAttrs(job *rivertype.JobRow) []any {
	return []any{
		slog.String("job_kind", job.Kind),
		slog.Int64("job_id", job.ID),
		slog.Int("attempt", job.Attempt),
		slog.Int("max_attempts", job.MaxAttempts),
	}
}
