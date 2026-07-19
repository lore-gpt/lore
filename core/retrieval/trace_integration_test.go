//go:build integration

package retrieval

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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

// TestHybridSpanUnsetOnNoActiveModel pins the deliberate carve-out in Hybrid.Retrieve: a project with no active
// embedding model (a fresh, pre-consolidation project) returns ErrNoActiveModel, which the pack read path treats
// as an empty distilled retrieval — a normal state, NOT a failure. So the retrieval.hybrid span must end UNSET,
// not red; otherwise every fresh project's first pack would light up trace-based error dashboards. This exercises
// the `!errors.Is(retErr, ErrNoActiveModel)` guard in the deferred span-end, which no prior test inspected (the
// existing no-active-model test asserts only the returned error).
func TestHybridSpanUnsetOnNoActiveModel(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	noModel := seedProject(ctx, t, st, "") // active_model_id NULL

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	spanCtx, root := tp.Tracer("test").Start(ctx, "ingest")

	emb := stubEmbedder{vecs: map[string][]float32{"q": {1, 0, 0, 0}}, dim: testDim, model: testModel}
	var retErr error
	if err := st.WithProject(spanCtx, noModel, func(tx pgx.Tx) error {
		_, _, retErr = NewHybrid(New(), emb).Retrieve(spanCtx, tx, noModel, "q", Filters{}, 10)
		return nil // swallow so WithProject commits and does not mask retErr
	}); err != nil {
		t.Fatalf("with project: %v", err)
	}
	root.End()

	if !errors.Is(retErr, ErrNoActiveModel) {
		t.Fatalf("Retrieve error = %v, want ErrNoActiveModel", retErr)
	}
	span := spanNamed(sr.Ended(), "retrieval.hybrid")
	if span == nil {
		t.Fatal("no retrieval.hybrid span recorded")
	}
	if got := span.Status().Code; got != codes.Unset {
		t.Errorf("retrieval.hybrid span status = %v on a no-active-model project, want Unset (empty distilled retrieval is not an error)", got)
	}
}
