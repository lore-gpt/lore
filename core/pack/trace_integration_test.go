//go:build integration

package pack

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/lore-gpt/lore/core/workmem"
)

// spanNamed returns the first recorded span with the given name, or nil.
func spanNamed(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// TestPackBuildTraceTreeUnderHTTPSpan pins the read-path span tree that the worker-side test cannot reach:
// an ambient (HTTP) span → pack.build → retrieval.hybrid, all sharing ONE trace id. Nesting depends on three
// ctx reassignments threading correctly (WithProject's closure captures the span ctx, pack.Build reassigns it,
// and passes it to hybrid.Retrieve); a regression that dropped any of them would orphan pack.build or
// retrieval.hybrid as a separate root trace with no test catching it. The project has no active model, which is
// irrelevant to nesting — retrieval.hybrid's span opens before the model lookup — so the tree forms regardless.
func TestPackBuildTraceTreeUnderHTTPSpan(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, "") // no active model: retrieval returns empty distilled, tree still forms
	run := seedRun(ctx, t, st, proj)
	seq := insertEvent(ctx, t, st, run, "planner", `{"note":"w"}`)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	spanCtx, root := tp.Tracer("test").Start(ctx, "ingest")

	p := New(newTestHybrid(), workmem.NewDisabled())
	runBuild(spanCtx, t, st, p, proj, run, Request{Query: "auth", MinSeq: seq, Limit: 10})
	root.End()

	spans := sr.Ended()
	build := spanNamed(spans, "pack.build")
	hybrid := spanNamed(spans, "retrieval.hybrid")
	if build == nil || hybrid == nil {
		t.Fatalf("missing spans: pack.build=%v retrieval.hybrid=%v", build != nil, hybrid != nil)
	}
	traceID := root.SpanContext().TraceID()
	if build.SpanContext().TraceID() != traceID || hybrid.SpanContext().TraceID() != traceID {
		t.Errorf("spans not in the ambient trace: build=%s hybrid=%s want=%s",
			build.SpanContext().TraceID(), hybrid.SpanContext().TraceID(), traceID)
	}
	if build.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Errorf("pack.build parent = %s, want the ambient span %s (orphaned build span)", build.Parent().SpanID(), root.SpanContext().SpanID())
	}
	if hybrid.Parent().SpanID() != build.SpanContext().SpanID() {
		t.Errorf("retrieval.hybrid parent = %s, want pack.build %s (orphaned hybrid span)", hybrid.Parent().SpanID(), build.SpanContext().SpanID())
	}
}

// TestPackBuildSpanUnsetOnMinSeqOutOfRange pins the pack.build carve-out: a read-your-writes barrier past the
// run head is a client-input 4xx (MinSeqOutOfRangeError), not a server fault, so the span must end UNSET rather
// than red — otherwise a caller mistake would inflate server-error trace dashboards.
func TestPackBuildSpanUnsetOnMinSeqOutOfRange(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, "")
	run := seedRun(ctx, t, st, proj)
	seq := insertEvent(ctx, t, st, run, "planner", `{"note":"w"}`)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	spanCtx, root := tp.Tracer("test").Start(ctx, "ingest")

	p := New(newTestHybrid(), workmem.NewDisabled())
	var buildErr error
	if err := st.WithProject(spanCtx, proj, func(tx pgx.Tx) error {
		_, buildErr = p.Build(spanCtx, tx, proj, run, Request{Query: "x", MinSeq: seq + 100, Limit: 10})
		return nil // swallow so WithProject commits and does not mask buildErr
	}); err != nil {
		t.Fatalf("with project: %v", err)
	}
	root.End()

	var badReq *MinSeqOutOfRangeError
	if !errors.As(buildErr, &badReq) {
		t.Fatalf("Build error = %v, want MinSeqOutOfRangeError", buildErr)
	}
	span := spanNamed(sr.Ended(), "pack.build")
	if span == nil {
		t.Fatal("no pack.build span recorded")
	}
	if got := span.Status().Code; got != codes.Unset {
		t.Errorf("pack.build span status = %v on a client 4xx, want Unset (a client mistake is not a server fault)", got)
	}
}
