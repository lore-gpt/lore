//go:build integration

package retrieval

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pgvector/pgvector-go"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

// TestRetrievalCarriesWhenAMemoryHappened proves every read path hands back memories.occurred_at — the exact
// scan, the index scan, and the lexical leg — and that an undated memory comes back undated. A path that
// dropped the column would still return the right memories in the right order, so nothing else would notice;
// the pack would just quietly lose its dates for whichever path a query happened to take.
func TestRetrievalCarriesWhenAMemoryHappened(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)

	may := time.Date(2023, time.May, 21, 14, 49, 0, 0, time.UTC)
	june := time.Date(2023, time.June, 2, 9, 0, 0, 0, time.UTC)
	want := map[string]pgtype.Timestamptz{
		"alpha deploy finished":     {Time: may, Valid: true},
		"beta rollout paused":       {},
		"gamma alpha retrospective": {Time: june, Valid: true},
	}
	vecs := map[string][]float32{
		"alpha deploy finished":     {1, 0, 0, 0},
		"beta rollout paused":       {0, 1, 0, 0},
		"gamma alpha retrospective": {0.6, 0.8, 0, 0},
	}
	for content, at := range want {
		var id pgtype.UUID
		if err := st.Pool.QueryRow(ctx,
			`INSERT INTO memories (project_id, kind, content, occurred_at) VALUES ($1, 'semantic', $2, $3) RETURNING id`,
			proj, content, at).Scan(&id); err != nil {
			t.Fatalf("insert memory %q: %v", content, err)
		}
		if _, err := db.New(st.Pool).UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
			ProjectID: proj, MemoryID: id, ModelID: testModel, Vec: pgvector.NewVector(vecs[content]),
		}); err != nil {
			t.Fatalf("insert embedding %q: %v", content, err)
		}
	}
	check := func(path string, got map[string]pgtype.Timestamptz) {
		t.Helper()
		if len(got) == 0 {
			t.Fatalf("%s: no results", path)
		}
		for content, at := range got {
			w := want[content]
			if at.Valid != w.Valid || (w.Valid && !at.Time.Equal(w.Time)) {
				t.Errorf("%s: %q occurred_at = %v, want %v", path, content, at, w)
			}
		}
	}
	query := []float32{1, 0, 0, 0}

	exact, path := retrieve(ctx, t, st, &Retriever{crossover: 100}, proj, query, Filters{}, 10)
	if path != PathExact {
		t.Fatalf("path = %q, want exact", path)
	}
	check("exact scan", resultDates(exact))

	if err := store.NewPgVectorIndex(st.Pool).EnsureIndex(ctx, proj, testDim); err != nil {
		t.Fatalf("ensure index: %v", err)
	}
	indexed, path := retrieve(ctx, t, st, &Retriever{crossover: 1}, proj, query, Filters{}, 10)
	if path == PathExact {
		t.Fatalf("path = %q, want an index-backed path", path)
	}
	check("index scan ("+string(path)+")", resultDates(indexed))

	var lexical []candidate
	if err := st.WithProject(ctx, proj, func(tx pgx.Tx) error {
		var e error
		lexical, e = lexicalLeg{}.retrieve(ctx, tx, proj, queryInput{text: "alpha"}, Filters{}, 10)
		return e
	}); err != nil {
		t.Fatalf("lexical leg: %v", err)
	}
	got := make(map[string]pgtype.Timestamptz, len(lexical))
	for _, c := range lexical {
		got[c.content] = c.occurredAt
	}
	if len(got) != 2 {
		t.Errorf("lexical leg matched %d memories for \"alpha\", want 2", len(got))
	}
	check("lexical leg", got)
}

func resultDates(results []Result) map[string]pgtype.Timestamptz {
	out := make(map[string]pgtype.Timestamptz, len(results))
	for _, r := range results {
		out[r.Content] = r.OccurredAt
	}
	return out
}
