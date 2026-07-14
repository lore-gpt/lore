//go:build integration

package queue_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
)

// TestPGPersisterWritesEmbeddingPerMemory proves the consolidation pass embeds every memory it stores:
// two distinct memories yield two embedding rows, each under the embedder's model and dimension, and no
// stored memory is left without an embedding. This is the M3-1 write half the retriever reads from.
func TestPGPersisterWritesEmbeddingPerMemory(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	emb := ext.FixtureEmbedder{}
	p := jobs.NewPGPersister(st, ext.LWW{}, emb)

	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         2,
		Memories: []jobs.MemoryWrite{
			{Kind: "semantic", Content: "auth is done", CreatedByAgent: "planner", SourceSeq: 1},
			{Kind: "semantic", Content: "search is pending", CreatedByAgent: "planner", SourceSeq: 2},
		},
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Two embedding rows, each carrying the fixture's model and dimension.
	var count, dims int
	var modelID string
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) OVER (), vector_dims(vec), model_id FROM embeddings WHERE project_id = $1 LIMIT 1`,
		proj.ID).Scan(&count, &dims, &modelID); err != nil {
		t.Fatalf("read embeddings: %v", err)
	}
	if count != 2 {
		t.Errorf("embeddings = %d, want 2 (one per memory)", count)
	}
	if dims != emb.Dim() {
		t.Errorf("embedding dimension = %d, want %d", dims, emb.Dim())
	}
	if modelID != emb.ModelID() {
		t.Errorf("embedding model_id = %q, want %q", modelID, emb.ModelID())
	}

	// No stored memory lacks an embedding.
	var orphans int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM memories m WHERE m.project_id = $1
		   AND NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.project_id = m.project_id AND e.memory_id = m.id)`,
		proj.ID).Scan(&orphans); err != nil {
		t.Fatalf("check orphan memories: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d stored memories have no embedding, want 0", orphans)
	}
}

// TestPGPersisterEmbedsDedupedMemoryOnce proves a duplicate restatement, which merges into one stored
// memory, produces exactly one embedding: only the first (inserted) memory is embedded, and the merge
// keeps that memory's vector rather than adding a second row.
func TestPGPersisterEmbedsDedupedMemoryOnce(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	p := jobs.NewPGPersister(st, ext.LWW{}, ext.FixtureEmbedder{})

	// Identical after normalization (casing, trailing punctuation, whitespace) → one memory.
	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         2,
		Memories: []jobs.MemoryWrite{
			{Kind: "semantic", Content: "Auth is done.", CreatedByAgent: "planner", SourceSeq: 1},
			{Kind: "semantic", Content: "auth is done", CreatedByAgent: "tester", SourceSeq: 2},
		},
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	var memCount, embCount int64
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE project_id = $1`, proj.ID).Scan(&memCount); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM embeddings WHERE project_id = $1`, proj.ID).Scan(&embCount); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if memCount != 1 || embCount != 1 {
		t.Errorf("memories=%d embeddings=%d, want 1 and 1 (the duplicate merges and re-embeds idempotently)", memCount, embCount)
	}
}

// wrongDimEmbedder claims dimension 8 but returns length-3 vectors, so the persister's dimension check
// must reject the pass before the vector reaches the dimensionless column (where it would only fail later,
// at index build).
type wrongDimEmbedder struct{}

func (wrongDimEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 2, 3}
	}
	return out, nil
}
func (wrongDimEmbedder) Dim() int        { return 8 }
func (wrongDimEmbedder) ModelID() string { return "wrong-dim" }

// TestPGPersisterRejectsWrongDimensionVector proves the app-level dimension assertion: a vector whose
// length does not match the embedder's declared dimension fails the pass. The check runs before the
// transaction opens, so nothing is written — no memory, no embedding, no checkpoint advance.
func TestPGPersisterRejectsWrongDimensionVector(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	p := jobs.NewPGPersister(st, ext.LWW{}, wrongDimEmbedder{})

	err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         1,
		Memories:           []jobs.MemoryWrite{{Kind: "semantic", Content: "auth is done", CreatedByAgent: "a", SourceSeq: 1}},
	})
	if err == nil {
		t.Fatal("a vector whose length does not match the embedder's dimension should fail the pass")
	}
	if !strings.Contains(err.Error(), "length") {
		t.Errorf("error = %v, want it to name the length mismatch", err)
	}

	var mem, emb, covered int64
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE project_id = $1`, proj.ID).Scan(&mem); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM embeddings WHERE project_id = $1`, proj.ID).Scan(&emb); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT covered_seq FROM runs WHERE id = $1`, run.ID).Scan(&covered); err != nil {
		t.Fatalf("read covered_seq: %v", err)
	}
	if mem != 0 || emb != 0 || covered != 0 {
		t.Errorf("after a rejected pass: memories=%d embeddings=%d covered_seq=%d, want 0/0/0 (full rollback)", mem, emb, covered)
	}
}
