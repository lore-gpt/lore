//go:build integration

package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/store/db"
)

// TestPGPersisterDatesMemoriesByTheirSourceEvent proves memories.occurred_at follows the event a memory
// cites, through every way the write path can touch a row: a fresh insert stores the source event's time, an
// exact restatement leaves it (as it leaves the content and provenance), a near-duplicate merge moves it to
// the incoming event along with the content it supersedes, and a memory with no known time stays undated
// rather than acquiring a placeholder.
func TestPGPersisterDatesMemoriesByTheirSourceEvent(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	q := db.New(st.Pool)

	const original = "the nightly build takes 40 minutes"
	const restated = "the nightly build takes forty minutes"
	const undated = "the release branch is cut on fridays"
	emb := scriptedEmbedder{dim: 4, vectors: map[string][]float32{
		original: {1, 0, 0, 0},
		restated: {0.98, 0.199, 0, 0}, // cosine ~0.98 with the original: a near duplicate
		undated:  {0, 0, 1, 0},
	}}
	p := jobs.NewPGPersister(st, ext.LWW{}, emb)
	entities := []jobs.EntityWrite{{Name: "nightly build", Type: "component"}}

	t1 := time.Date(2023, time.May, 21, 14, 49, 0, 0, time.UTC)
	t2 := time.Date(2023, time.June, 2, 9, 0, 0, 0, time.UTC)
	t3 := time.Date(2023, time.July, 9, 18, 30, 0, 0, time.UTC)

	events := make([]db.Event, 3)
	for i := range events {
		ev, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "agent", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatalf("insert event %d: %v", i+1, err)
		}
		events[i] = ev
	}
	write := func(content string, ev db.Event, when time.Time) jobs.MemoryWrite {
		return jobs.MemoryWrite{
			Kind: "semantic", Content: content, CreatedByAgent: "agent",
			SourceEventID: ev.ID, SourceSeq: ev.Seq, OccurredAt: when,
		}
	}
	persist := func(expected int64, ev db.Event, memories ...jobs.MemoryWrite) {
		t.Helper()
		if err := p.Persist(ctx, jobs.PersistInput{
			ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: expected, CoveredSeq: ev.Seq,
			Memories: memories, Entities: entities,
		}); err != nil {
			t.Fatalf("persist through seq %d: %v", ev.Seq, err)
		}
	}
	dateOf := func(content string) pgtype.Timestamptz {
		t.Helper()
		var at pgtype.Timestamptz
		if err := st.Pool.QueryRow(ctx,
			`SELECT occurred_at FROM memories WHERE project_id = $1 AND content = $2`, proj.ID, content).Scan(&at); err != nil {
			t.Fatalf("read occurred_at of %q: %v", content, err)
		}
		return at
	}

	// A fresh insert is dated by its source event; one with no known time is stored undated.
	persist(0, events[0], write(original, events[0], t1), write(undated, events[0], time.Time{}))
	if got := dateOf(original); !got.Valid || !got.Time.Equal(t1) {
		t.Errorf("inserted memory occurred_at = %v, want %v", got, t1)
	}
	if got := dateOf(undated); got.Valid {
		t.Errorf("a memory with no known time was stored with occurred_at = %v; it should be NULL", got.Time)
	}

	// An exact restatement later keeps the original date, as it keeps the original content and provenance.
	persist(events[0].Seq, events[1], write(original, events[1], t2))
	if got := dateOf(original); !got.Valid || !got.Time.Equal(t1) {
		t.Errorf("after an exact restatement occurred_at = %v, want the original %v", got, t1)
	}

	// A near duplicate supersedes the row, and the row is then dated by the event it now cites.
	persist(events[1].Seq, events[2], write(restated, events[2], t3))
	var count int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM memories WHERE project_id = $1 AND content IN ($2, $3)`, proj.ID, original, restated).
		Scan(&count); err != nil {
		t.Fatalf("count merged rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows for the build memory = %d, want 1 (the near duplicate merged into it)", count)
	}
	if got := dateOf(restated); !got.Valid || !got.Time.Equal(t3) {
		t.Errorf("after a near-duplicate merge occurred_at = %v, want the incoming event's %v", got, t3)
	}
}
