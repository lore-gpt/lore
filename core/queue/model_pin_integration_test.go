//go:build integration

package queue_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/store/db"
)

// TestPGPersisterPinsActiveModelOnFirstEmbed proves the write path pins a project's active embedding model to
// the running embedder on the first pass that stores a vector: the column is NULL beforehand and equals the
// embedder's model afterward. This is what unblocks recall — a project adopts the deployment's model with no
// operator step.
func TestPGPersisterPinsActiveModelOnFirstEmbed(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	emb := ext.FixtureEmbedder{}
	p := jobs.NewPGPersister(st, ext.LWW{}, emb)

	var before *string
	if err := st.Pool.QueryRow(ctx, `SELECT active_model_id FROM projects WHERE id = $1`, proj.ID).Scan(&before); err != nil {
		t.Fatalf("read active model before: %v", err)
	}
	if before != nil {
		t.Fatalf("active_model_id before first embed = %q, want NULL", *before)
	}

	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         1,
		Memories:           []jobs.MemoryWrite{{Kind: "semantic", Content: "auth is done", CreatedByAgent: "a", SourceSeq: 1}},
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	var after *string
	if err := st.Pool.QueryRow(ctx, `SELECT active_model_id FROM projects WHERE id = $1`, proj.ID).Scan(&after); err != nil {
		t.Fatalf("read active model after: %v", err)
	}
	if after == nil || *after != emb.ModelID() {
		t.Errorf("active_model_id after first embed = %v, want %q", after, emb.ModelID())
	}
}

// TestPGPersisterRejectsModelMismatch proves the write-side mismatch guard: a project already pinned to a
// different model than the running embedder fails the pass loudly (ErrModelMismatch) before any vector is
// written in a second model's space — the whole pass rolls back and the pin is unchanged.
func TestPGPersisterRejectsModelMismatch(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	if _, err := st.Pool.Exec(ctx, `UPDATE projects SET active_model_id = 'other-model' WHERE id = $1`, proj.ID); err != nil {
		t.Fatalf("pin other model: %v", err)
	}
	p := jobs.NewPGPersister(st, ext.LWW{}, ext.FixtureEmbedder{}) // embedder model "fixture-embed-v1"

	err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         1,
		Memories:           []jobs.MemoryWrite{{Kind: "semantic", Content: "auth is done", CreatedByAgent: "a", SourceSeq: 1}},
	})
	if !errors.Is(err, jobs.ErrModelMismatch) {
		t.Fatalf("persist with a mismatched embedder: err = %v, want ErrModelMismatch", err)
	}

	// Full rollback and the pin untouched: no memory, no embedding, checkpoint at 0, model still 'other-model'.
	var mem, emb, covered int64
	var active *string
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE project_id = $1`, proj.ID).Scan(&mem); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM embeddings WHERE project_id = $1`, proj.ID).Scan(&emb); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT covered_seq FROM runs WHERE id = $1`, run.ID).Scan(&covered); err != nil {
		t.Fatalf("read covered_seq: %v", err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT active_model_id FROM projects WHERE id = $1`, proj.ID).Scan(&active); err != nil {
		t.Fatalf("read active model: %v", err)
	}
	if mem != 0 || emb != 0 || covered != 0 {
		t.Errorf("after a rejected mismatch pass: memories=%d embeddings=%d covered_seq=%d, want 0/0/0 (full rollback)", mem, emb, covered)
	}
	if active == nil || *active != "other-model" {
		t.Errorf("active_model_id after rejected pass = %v, want unchanged 'other-model'", active)
	}
}

// TestPinActiveModelIfUnsetSingleWinnerUnderRace proves the pin is first-wins under real concurrency: many
// passes racing to pin the same NULL-model project leave EXACTLY ONE with rows-affected 1 (the one that chose
// the model) and one consistent value. This is what makes the rows-affected result a trustworthy one-time
// signal — a snapshot-read flag would report several winners because each racer's pre-update snapshot sees
// NULL, whereas the conditional UPDATE's re-checked predicate is false for every loser.
func TestPinActiveModelIfUnsetSingleWinnerUnderRace(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, _ := seedProjectRun(ctx, t, st)
	model := "fixture-embed-v1"

	const racers = 8
	var wg sync.WaitGroup
	pinned := make([]int64, racers)
	errs := make([]error, racers)
	startGate := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-startGate // release all goroutines together to maximise the race
			errs[i] = st.WithProject(ctx, proj.ID, func(tx pgx.Tx) error {
				n, e := db.New(tx).PinActiveModelIfUnset(ctx, db.PinActiveModelIfUnsetParams{ProjectID: proj.ID, ModelID: &model})
				pinned[i] = n
				return e
			})
		}(i)
	}
	close(startGate)
	wg.Wait()

	var winners int64
	for i := 0; i < racers; i++ {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		winners += pinned[i]
	}
	if winners != 1 {
		t.Errorf("first-pin winners (rows affected == 1) = %d, want exactly 1", winners)
	}

	var active *string
	if err := st.Pool.QueryRow(ctx, `SELECT active_model_id FROM projects WHERE id = $1`, proj.ID).Scan(&active); err != nil {
		t.Fatalf("read active model: %v", err)
	}
	if active == nil || *active != model {
		t.Errorf("active_model_id = %v, want %q (one consistent winner)", active, model)
	}
}
