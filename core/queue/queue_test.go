package queue

import (
	"context"
	"testing"

	"github.com/riverqueue/river/rivertype"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
)

// An insert-only client (the shape New returns) must refuse to Start, so the
// server can never silently turn into a worker. This locks the guard in CI
// rather than a comment. No database needed: Start rejects before touching it.
func TestInsertOnlyClientCannotStart(t *testing.T) {
	q := &Queue{} // worker == false, i.e. what New produces
	if err := q.Start(context.Background()); err == nil {
		t.Error("Start() on an insert-only client returned nil, want an error")
	}
}

// otelMiddleware must build with a nil TracerProvider (coerced to the no-op, so a caller can pass the tracer
// unconditionally with tracing off) AND wire propagation on BOTH boundaries: the enqueue side (JobInsertMiddleware,
// which injects the trace context into the job's Metadata) and the work side (WorkerMiddleware, which re-links the
// span). This test pins ONLY that shape — that the constructed middleware satisfies both River interfaces (a
// missing one would break the trace across the queue). The propagation/trace-only INVARIANTS the middleware
// depends on are pinned by TestOtelMiddlewareConfigInvariants (the config) and TestExtractRunCoalescesWith-
// TracePropagation (the wire behaviour), not by these interface assertions — otelriver's type satisfies both
// interfaces for ANY config, so a wrong config would still pass here.
func TestOtelMiddlewareIsBidirectional(t *testing.T) {
	mw := otelMiddleware(nil) // nil must not panic: coerced to the no-op TracerProvider
	if mw == nil {
		t.Fatal("otelMiddleware(nil) = nil, want a middleware")
	}
	if _, ok := mw.(rivertype.JobInsertMiddleware); !ok {
		t.Error("otel middleware does not implement JobInsertMiddleware: trace context is never injected on enqueue")
	}
	if _, ok := mw.(rivertype.WorkerMiddleware); !ok {
		t.Error("otel middleware does not implement WorkerMiddleware: the worker span is never linked to the enqueue")
	}
}

// TestOtelMiddlewareConfigInvariants pins the two config invariants otelriver's behaviour depends on — the ones
// the interface assertions above CANNOT see (otelriver's type satisfies both interfaces regardless of config):
//   - EnableTracePropagation MUST be true, or otelriver stops injecting the W3C trace context into job Metadata,
//     silently breaking cross-queue trace linking (a coalescing-safe metadata write, not an args write).
//   - MeterProvider MUST be the no-op. A nil MeterProvider falls back to the GLOBAL meter provider, so when a
//     real one is installed otelriver would emit river.* job metrics that double-count the Prometheus job
//     metrics this repo already records in jobMetricsMiddleware.
//
// A nil TracerProvider must coerce to a non-nil no-op so a caller can pass it unconditionally with tracing off.
func TestOtelMiddlewareConfigInvariants(t *testing.T) {
	cfg := otelMiddlewareConfig(nil)
	if !cfg.EnableTracePropagation {
		t.Error("EnableTracePropagation is false: the trace context is never injected into job metadata, breaking the trace across the queue")
	}
	if _, ok := cfg.MeterProvider.(metricnoop.MeterProvider); !ok {
		t.Errorf("MeterProvider = %T, want the no-op provider: a nil/global meter would double-count the Prometheus job metrics", cfg.MeterProvider)
	}
	if cfg.TracerProvider == nil {
		t.Error("TracerProvider is nil: a nil TracerProvider must coerce to the no-op, else otelriver uses the global provider")
	}
}
