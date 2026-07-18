//go:build integration

package queue_test

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/store/db"
)

// TestConsolidationDedupFunnelMetrics drives the dedup funnel with a REAL registry and controlled embeddings
// and asserts the per-decision similarity histogram and the advisory-lock-wait histogram. Two separate entity
// buckets avoid vector interaction: "auth" exercises a near_merge (cosine ~0.98), "db" a gray_zone (~0.88).
// The scriptedEmbedder is needed because the deterministic fixture embedder can't produce a controlled
// high-cosine pair. Metrics are recorded after each pass commits.
func TestConsolidationDedupFunnelMetrics(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	q := db.New(st.Pool)
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	const (
		authOrig = "auth login is broken"
		authNear = "auth sign-in is failing"
		dbOrig   = "db writes are slow"
		dbGray   = "db persistence lags sometimes"
	)
	emb := scriptedEmbedder{dim: 4, vectors: map[string][]float32{
		authOrig: {1, 0, 0, 0},
		authNear: {0.98, 0.199, 0, 0},  // cosine ~0.98 with authOrig → near_merge (>= 0.92)
		dbOrig:   {1, 0, 0, 0},         // same vector, but a different entity bucket → never compared to auth
		dbGray:   {0.88, 0.4751, 0, 0}, // cosine ~0.88 with dbOrig → gray_zone (in [0.85, 0.92))
	}}
	p := jobs.NewPGPersister(st, ext.LWW{}, emb, jobs.WithPersisterMetrics(m))

	events := make([]db.Event, 4)
	for i := range events {
		ev, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
		events[i] = ev
	}
	pass := func(exp, cov int64, entity, content string) {
		t.Helper()
		if err := p.Persist(ctx, jobs.PersistInput{
			ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: exp, CoveredSeq: cov,
			Memories: []jobs.MemoryWrite{{Kind: "semantic", Content: content, CreatedByAgent: "a", SourceEventID: events[cov-1].ID, SourceSeq: cov}},
			Entities: []jobs.EntityWrite{{Name: entity, Type: "service"}},
		}); err != nil {
			t.Fatalf("persist (%s/%s): %v", entity, content, err)
		}
	}
	pass(0, 1, "auth", authOrig)         // insert (no candidate)
	pass(1, 2, "auth", authNear)         // near_merge vs authOrig
	pass(events[1].Seq, 3, "db", dbOrig) // insert (empty db bucket)
	pass(events[2].Seq, 4, "db", dbGray) // gray_zone vs dbOrig

	near := findMetric(t, reg, "lore_consolidation_similarity_cosine", map[string]string{"decision": "near_merge"})
	if near == nil || near.GetHistogram().GetSampleCount() != 1 {
		t.Fatalf("near_merge similarity: want 1 sample, got %v", near)
	}
	if s := near.GetHistogram().GetSampleSum(); s < 0.97 || s > 0.99 {
		t.Errorf("near_merge cosine sum = %v, want ~0.98", s)
	}
	gray := findMetric(t, reg, "lore_consolidation_similarity_cosine", map[string]string{"decision": "gray_zone"})
	if gray == nil || gray.GetHistogram().GetSampleCount() != 1 {
		t.Fatalf("gray_zone similarity: want 1 sample, got %v", gray)
	}
	if s := gray.GetHistogram().GetSampleSum(); s < 0.86 || s > 0.90 {
		t.Errorf("gray_zone cosine sum = %v, want ~0.88", s)
	}
	// Every pass touched an entity, so each acquired the advisory lock: four observations.
	if lw := findMetric(t, reg, "lore_consolidation_lock_wait_seconds", nil); lw == nil || lw.GetHistogram().GetSampleCount() != 4 {
		t.Errorf("lock-wait: want 4 samples (one per pass), got %v", lw)
	}
}
