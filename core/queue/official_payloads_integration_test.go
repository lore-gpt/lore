//go:build integration

package queue_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/pack"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/retrieval"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
)

// TestOfflineDefaultDistilsShippedPayloadShapes is a regression lock on the offline default: every
// write path we ship — the TypeScript and Python SDKs, the MCP server's memory_write, and the
// quickstart's curl — sends a payload keyed by "content" or "note", and none of them carries the
// fixture's own "memory" key. When the fixture read only "memory", all of them distilled nothing:
// a first run against the default stack produced an empty memory list and a pack that never left
// the raw tail, with no error anywhere to explain it.
//
// The assertion therefore spans the whole chain rather than the extractor alone — ingest, the
// worker's consolidation pass, the stored memory, and the rendered pack — because the failure was
// only visible end to end. A third event with an unrecognised key is the control: the aliases must
// widen the convention to the shapes we ship, not make the fixture distil arbitrary payloads.
func TestOfflineDefaultDistilsShippedPayloadShapes(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	w, err := queue.NewWorker(st, ext.FixtureExtractor{}, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), metrics.NewNoop(), tracenoop.NewTracerProvider())
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
	q := db.New(st.Pool)

	const (
		sdkText  = "auth flow moved to v2 - PR 42 merged"
		curlText = "the deploy freeze starts Friday"
	)
	for _, payload := range []string{
		`{"content":"` + sdkText + `"}`, // TS/Python SDK write(), MCP memory_write(content:)
		`{"note":"` + curlText + `"}`,   // the quickstart's curl body
		`{"observation":"not a key"}`,   // control: unrecognised key distils nothing
	} {
		if _, err := q.InsertEvent(ctx, db.InsertEventParams{
			RunID: run.ID, AgentID: "researcher", Payload: []byte(payload),
		}); err != nil {
			t.Fatalf("insert event %s: %v", payload, err)
		}
	}

	enqueueExtract(ctx, t, w, st, uuid.UUID(proj.ID.Bytes).String(), uuid.UUID(run.ID.Bytes).String())
	waitForMemoryCount(ctx, t, st, proj.ID, 2, 30*time.Second)

	// Exactly two: the control event must not have contributed a third.
	var got []string
	rows, err := st.Pool.Query(ctx,
		`SELECT content FROM memories WHERE project_id = $1 ORDER BY content`, proj.ID)
	if err != nil {
		t.Fatalf("list memories: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan memory: %v", err)
		}
		got = append(got, c)
	}
	if rows.Err() != nil {
		t.Fatalf("iterate memories: %v", rows.Err())
	}
	want := []string{sdkText, curlText}
	if len(got) != len(want) {
		t.Fatalf("memories = %q, want exactly %q (the unrecognised key must distil nothing)", got, want)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("memory %q missing from %q", w, got)
		}
	}

	// The pack is the surface an agent actually reads: a distilled memory that never reaches the
	// Semantic section is invisible in practice, so assert the rendered text, not just the row.
	p := pack.New(retrieval.NewHybrid(retrieval.New(), ext.FixtureEmbedder{}), workmem.NewDisabled())
	var res pack.Result
	if err := st.WithProject(ctx, proj.ID, func(tx pgx.Tx) error {
		var e error
		res, e = p.Build(ctx, tx, proj.ID, run.ID, pack.Request{Query: "auth deploy", Limit: 10})
		return e
	}); err != nil {
		t.Fatalf("build pack: %v", err)
	}

	if !strings.Contains(res.Text, "## Semantic") {
		t.Fatalf("pack has no Semantic section; text:\n%s", res.Text)
	}
	semantic := res.Text[strings.Index(res.Text, "## Semantic"):]
	if i := strings.Index(semantic[len("## Semantic"):], "\n##"); i >= 0 {
		semantic = semantic[:len("## Semantic")+i]
	}
	for _, wantText := range want {
		if !strings.Contains(semantic, wantText) {
			t.Errorf("pack Semantic section missing %q; section:\n%s", wantText, semantic)
		}
	}
	if len(res.Sources) < len(want) {
		t.Errorf("pack sources = %d, want >= %d", len(res.Sources), len(want))
	}
}
