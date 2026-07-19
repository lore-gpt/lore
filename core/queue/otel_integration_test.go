//go:build integration

package queue_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
)

// TestExtractRunCoalescesWithTracePropagation proves the tracing wiring does not defeat the per-run unique job:
// with a REAL (recording) tracer provider — so otelriver actually injects W3C trace context on enqueue — many
// enqueues for one run still collapse into a single extract_run job. otelriver injects the context into the job's
// Metadata, never its args, and River's unique key is args-derived, so coalescing is preserved.
//
// The count check alone is insufficient: it would also pass if propagation were silently off (nothing injected).
// So the test also asserts the surviving job's metadata carries a traceparent — pinning that the context landed
// in the coalescing-safe location (metadata) while the args, and thus the unique key, were left untouched.
func TestExtractRunCoalescesWithTracePropagation(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	// A real SDK provider (default sampler records the root span) so the enqueue has a live trace context to
	// inject; a no-op provider would inject nothing and the metadata assertion below could not distinguish
	// "coalescing-safe" from "propagation off".
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	q, err := queue.New(st.Pool, tp)
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}

	// Enqueue under an active, recording span so otelriver injects its context into the job metadata.
	spanCtx, span := tp.Tracer("test").Start(ctx, "enqueue-batch")
	defer span.End()

	enqueue := func(project, run string) {
		tx, err := st.Pool.Begin(spanCtx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := q.EnqueueExtract(spanCtx, tx, project, run); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := tx.Commit(spanCtx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	project := uuid.NewString()
	runA, runB := uuid.NewString(), uuid.NewString()
	enqueue(project, runA)
	enqueue(project, runA) // same run -> coalesced even though each enqueue injects trace context into metadata
	enqueue(project, runA) // coalesced
	enqueue(project, runB) // distinct run -> its own job

	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'extract_run'`).Scan(&n); err != nil {
		t.Fatalf("count extract_run jobs: %v", err)
	}
	if n != 2 {
		t.Errorf("extract_run jobs = %d, want 2 (trace context in metadata must not defeat per-run coalescing)", n)
	}

	// Prove propagation is actually live: at least one surviving job's metadata carries a traceparent. Had
	// otelriver perturbed the args instead, coalescing would already have failed above; asserting the traceparent
	// here rules out the other false-pass — that propagation was silently disabled.
	rows, err := st.Pool.Query(ctx, `SELECT coalesce(metadata::text, '') FROM river_job WHERE kind = 'extract_run'`)
	if err != nil {
		t.Fatalf("read job metadata: %v", err)
	}
	defer rows.Close()
	var metaCarriesTraceparent bool
	for rows.Next() {
		var meta string
		if err := rows.Scan(&meta); err != nil {
			t.Fatalf("scan metadata: %v", err)
		}
		if strings.Contains(meta, "traceparent") {
			metaCarriesTraceparent = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !metaCarriesTraceparent {
		t.Error("no extract_run job metadata carried a traceparent: trace propagation did not inject on enqueue")
	}
}

// TestWorkerTraceNestsPhasesUnderOneTrace is the end-to-end trace-shape guard: one extraction pass, driven to
// completion against real River + ParadeDB with a recording tracer, must produce the four worker-side phases —
// the framework work span, extraction, consolidation, and embedding — as a SINGLE parent-linked tree under ONE
// trace id, so an operator can open one trace and see the whole job. It anchors on the embed span (only the
// committing pass embeds) and walks parents up, which is robust to extra debounce-snooze passes: each Work
// invocation is its own linked root trace, so the committing pass's trace is isolated cleanly.
//
// The ingest HTTP request is deliberately a SEPARATE, linked trace, not part of this tree: River jobs are async
// (a pass can run minutes after the enqueue), so otelriver links the work span to the enqueue rather than
// parenting it — a shared trace id across the async boundary would produce misleading, unbounded traces. To pin
// that contract directly (not just as prose), the job is enqueued under a recording INGEST span and the test
// asserts the framework work span carries exactly one LINK back to that ingest span, in a DIFFERENT trace.
func TestWorkerTraceNestsPhasesUnderOneTrace(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	w, err := queue.NewWorker(st, ext.FixtureExtractor{}, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), metrics.NewNoop(), tp)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = w.Stop(stopCtx)
	})

	proj, run := seedProjectRun(ctx, t, st)
	if _, err := db.New(st.Pool).InsertEvent(ctx, db.InsertEventParams{
		RunID: run.ID, AgentID: "planner", Payload: []byte(`{"memory":"trace me"}`),
	}); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	// Enqueue under a recording INGEST span so otelriver injects its context into job metadata; on Work it links
	// (not parents) the worker span back to this ingest span — the async-boundary contract asserted at the end.
	ingestCtx, ingestSpan := tp.Tracer("test").Start(ctx, "ingest")
	enqueueExtract(ingestCtx, t, w, st, uuid.UUID(proj.ID.Bytes).String(), uuid.UUID(run.ID.Bytes).String())
	ingestSpan.End()
	waitForCompletedExtractJobs(ctx, t, st, 1, 20*time.Second)
	_ = tp.ForceFlush(ctx)

	spans := sr.Ended()

	// Anchor: the embed span of the committing pass, and the map of every span in its trace by span id.
	var embed sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "consolidation.embed" {
			embed = s
			break
		}
	}
	if embed == nil {
		t.Fatal("no consolidation.embed span recorded: the committing pass did not embed (or the span is unwired)")
	}
	traceID := embed.SpanContext().TraceID()
	bySpanID := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		if s.SpanContext().TraceID() == traceID {
			bySpanID[s.SpanContext().SpanID()] = s
		}
	}

	// Walk parents up from embed, asserting each phase's name in order. This proves both membership in one trace
	// and the exact nesting: embed under consolidation.persist under extract.run under the framework work span.
	parentChain := []string{"consolidation.embed", "consolidation.persist", "extract.run", "river.work"}
	cur := embed
	for i, wantName := range parentChain {
		if got := cur.Name(); got != wantName {
			t.Fatalf("phase %d in the parent chain = %q, want %q", i, got, wantName)
		}
		if cur.SpanContext().TraceID() != traceID {
			t.Errorf("span %q has trace id %s, want %s (all phases must share one trace)", cur.Name(), cur.SpanContext().TraceID(), traceID)
		}
		if i == len(parentChain)-1 {
			break // river.work is the trace root; cur now holds it — its link to the ingest trace is asserted below
		}
		parent, ok := bySpanID[cur.Parent().SpanID()]
		if !ok {
			t.Fatalf("span %q has no parent in trace %s, want a %q parent", cur.Name(), traceID, parentChain[i+1])
		}
		cur = parent
	}

	// The framework work span (cur == river.work) links, not parents, to the enqueue: otelriver records exactly
	// one link back to the ingest span, in a DIFFERENT (separate) trace — the async-boundary contract. A regression
	// that broke link creation (EnableTracePropagation off, or metadata extraction failing) would drop this link.
	links := cur.Links()
	if len(links) != 1 {
		t.Fatalf("river.work has %d links, want exactly 1 (the async link back to the ingest/enqueue span)", len(links))
	}
	if lt := links[0].SpanContext.TraceID(); lt != ingestSpan.SpanContext().TraceID() {
		t.Errorf("river.work link trace id = %s, want the ingest trace %s", lt, ingestSpan.SpanContext().TraceID())
	}
	if links[0].SpanContext.TraceID() == traceID {
		t.Error("river.work link points at its own worker trace; want the SEPARATE ingest trace (async link, not parent)")
	}
}

// TestConsolidationPersistSpanUnsetOnCheckpointConflict pins the consolidation.persist carve-out: a checkpoint
// conflict is an EXPECTED outcome of River's at-least-once delivery (a concurrent pass won the compare-and-swap),
// which the caller swallows and which has its own metric bucket (checkpoint_conflict, not error). So the span must
// end UNSET, matching the clean parent span and the metrics layer — otherwise every routine concurrent
// double-delivery would spuriously redden the trace. The first pass runs OFF the recorder (background ctx → no-op
// span) so exactly one consolidation.persist span — the conflicting second pass — is captured.
func TestConsolidationPersistSpanUnsetOnCheckpointConflict(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	p := jobs.NewPGPersister(st, ext.LWW{}, ext.FixtureEmbedder{})

	// Pass 1 advances the checkpoint 0 -> 1 (background ctx: its span is a non-recording no-op, not captured).
	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: 0, CoveredSeq: 1,
		Memories: []jobs.MemoryWrite{{Kind: "semantic", Content: "auth done", CreatedByAgent: "a", SourceSeq: 1}},
	}); err != nil {
		t.Fatalf("first persist: %v", err)
	}

	// Pass 2 starts from the now-stale ExpectedCoveredSeq 0, so the checkpoint CAS matches no row and the whole
	// pass rolls back with the conflict — recorded under the ambient span so its status is captured.
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	spanCtx, root := tp.Tracer("test").Start(ctx, "worker")
	err := p.Persist(spanCtx, jobs.PersistInput{
		ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: 0, CoveredSeq: 2,
		Memories: []jobs.MemoryWrite{{Kind: "semantic", Content: "search pending", CreatedByAgent: "a", SourceSeq: 2}},
	})
	root.End()

	if err == nil {
		t.Fatal("second persist from a stale checkpoint = nil error, want a checkpoint conflict")
	}
	span := spanByName(sr.Ended(), "consolidation.persist")
	if span == nil {
		t.Fatal("no consolidation.persist span recorded for the conflicting pass")
	}
	if got := span.Status().Code; got != codes.Unset {
		t.Errorf("consolidation.persist span status = %v on a checkpoint conflict, want Unset (an expected concurrent double-delivery is not an error)", got)
	}
}

// spanByName returns the first recorded span with the given name, or nil.
func spanByName(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}
