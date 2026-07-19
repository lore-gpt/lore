// Package obs holds Lore's shared observability helpers. Metrics live in core/metrics; this package carries the
// tracing seam so business code can open spans without threading a TracerProvider through every constructor.
package obs

import (
	"context"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationName is the tracer (instrumentation-scope) name attached to every Lore business span.
const instrumentationName = "github.com/lore-gpt/lore"

// StartSpan opens a business span named `name` as a child of whatever span is already in ctx — the otelhttp HTTP
// server span on the read path, or the otelriver worker span on the job path. It reads the TracerProvider from the
// ambient span, so business spans use the SAME provider that composition injected, with no global lookup and no
// constructor threading. With tracing off (the default) the ambient span is the no-op, its provider is the no-op,
// and this is a cheap non-recording span that exports nothing.
//
// Span names are the business verb vocabulary shared with the metric subsystems (extract, consolidation, pack,
// retrieval). Callers attach counts and identifiers as attributes; they must never attach memory content, a query
// string, or an event payload — a span is not a place for user data.
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	tp := trace.SpanFromContext(ctx).TracerProvider()
	return tp.Tracer(instrumentationName).Start(ctx, name, opts...)
}

// End finalizes span, recording err as the span's error status when non-nil. It centralizes the record-then-end
// idiom so every business span reports failure the same way. A nil err ends the span cleanly (unset status).
func End(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}
