//go:build integration

package retrieval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

const (
	paradeDBImage = "paradedb/paradedb:0.24.2-pg17"
	testModel     = "fixture-embed-v1"
	testDim       = 4
)

// migratedStore starts a ParadeDB container, applies the store migrations, and returns an open store.
func migratedStore(ctx context.Context, t *testing.T) *store.Store {
	t.Helper()
	ctr, err := tcpostgres.Run(ctx, paradeDBImage,
		tcpostgres.WithDatabase("lore"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start paradedb: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// seedProject creates an org + project + partitions and sets its active model (empty leaves it NULL).
func seedProject(ctx context.Context, t *testing.T, st *store.Store, activeModel string) pgtype.UUID {
	t.Helper()
	q := db.New(st.Pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "p"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := store.CreateProjectPartitions(ctx, st.Pool, proj.ID); err != nil {
		t.Fatalf("create partitions: %v", err)
	}
	if activeModel != "" {
		if _, err := st.Pool.Exec(ctx, `UPDATE projects SET active_model_id = $2 WHERE id = $1`, proj.ID, activeModel); err != nil {
			t.Fatalf("set active model: %v", err)
		}
	}
	return proj.ID
}

// insertMem inserts a live memory (with scope tags and trust tier) plus its embedding under testModel,
// returning the memory id. Direct SQL because InsertMemory does not set scope_keys/trust_tier.
func insertMem(ctx context.Context, t *testing.T, st *store.Store, projectID pgtype.UUID, content, tier string, scopes []string, vec []float32) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO memories (project_id, kind, content, scope_keys, trust_tier) VALUES ($1,'semantic',$2,$3,$4) RETURNING id`,
		projectID, content, scopes, tier).Scan(&id); err != nil {
		t.Fatalf("insert memory %q: %v", content, err)
	}
	if _, err := db.New(st.Pool).UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: projectID, MemoryID: id, ModelID: testModel, Vec: pgvector.NewVector(vec),
	}); err != nil {
		t.Fatalf("insert embedding %q: %v", content, err)
	}
	return id
}

// retrieve runs the retriever inside a tenant transaction and returns its results and path.
func retrieve(ctx context.Context, t *testing.T, st *store.Store, r *Retriever, projectID pgtype.UUID, queryVec []float32, filters Filters, limit int) ([]Result, Path) {
	t.Helper()
	var results []Result
	var path Path
	err := st.WithProject(ctx, projectID, func(tx pgx.Tx) error {
		var e error
		results, path, e = r.Retrieve(ctx, tx, projectID, pgvector.NewVector(queryVec), filters, limit)
		return e
	})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	return results, path
}

func contents(results []Result) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.Content
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRetrieveExactFiltersAndOrdering proves the exact path: results come back ordered by ascending cosine
// distance, the scope filter is an overlap (a memory in any requested scope is visible), an empty scope is
// project-wide, and quarantine-tier memories are excluded by default but included behind the internal flag.
func TestRetrieveExactFiltersAndOrdering(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)

	// query = [1,0,0,0]; distances below are 1 - cosine.
	insertMem(ctx, t, st, proj, "auth is broken", "normal", []string{"run:r1"}, []float32{1, 0, 0, 0})          // d 0.0
	insertMem(ctx, t, st, proj, "search is slow", "normal", []string{"run:r1"}, []float32{0, 1, 0, 0})          // d 1.0
	insertMem(ctx, t, st, proj, "deploy is done", "normal", []string{"run:r2"}, []float32{0.6, 0.8, 0, 0})      // d 0.4
	insertMem(ctx, t, st, proj, "secret leaked", "quarantine", []string{"run:r1"}, []float32{0.9, 0.436, 0, 0}) // d ~0.1

	r := New()
	query := []float32{1, 0, 0, 0}

	// r1 scope, default (no quarantine): auth then search; deploy (r2) and secret (quarantine) excluded.
	res, path := retrieve(ctx, t, st, r, proj, query, Filters{Scopes: []string{"run:r1"}}, 10)
	if path != PathExact {
		t.Errorf("path = %q, want exact (small set)", path)
	}
	if got := contents(res); !equalStrings(got, []string{"auth is broken", "search is slow"}) {
		t.Errorf("r1 default = %v, want [auth is broken, search is slow] (ordered, no quarantine, no r2)", got)
	}

	// r1 scope, include quarantine: auth, secret, search (by distance).
	res, _ = retrieve(ctx, t, st, r, proj, query, Filters{Scopes: []string{"run:r1"}, IncludeQuarantine: true}, 10)
	if got := contents(res); !equalStrings(got, []string{"auth is broken", "secret leaked", "search is slow"}) {
		t.Errorf("r1 include-quarantine = %v, want [auth, secret, search] by distance", got)
	}

	// empty scope = project-wide (default no quarantine): auth, deploy, search; secret excluded.
	res, _ = retrieve(ctx, t, st, r, proj, query, Filters{}, 10)
	if got := contents(res); !equalStrings(got, []string{"auth is broken", "deploy is done", "search is slow"}) {
		t.Errorf("empty scope = %v, want project-wide [auth, deploy, search], quarantine excluded", got)
	}

	// r2 scope only: deploy.
	res, _ = retrieve(ctx, t, st, r, proj, query, Filters{Scopes: []string{"run:r2"}}, 10)
	if got := contents(res); !equalStrings(got, []string{"deploy is done"}) {
		t.Errorf("r2 scope = %v, want [deploy is done] only", got)
	}
}

// TestRetrieveNoActiveModel proves a project with no active embedding model is a loud, typed error, not a
// silent empty result.
func TestRetrieveNoActiveModel(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, "") // active_model_id NULL

	err := st.WithProject(ctx, proj, func(tx pgx.Tx) error {
		_, _, e := New().Retrieve(ctx, tx, proj, pgvector.NewVector([]float32{1, 0, 0, 0}), Filters{}, 10)
		return e
	})
	if !errors.Is(err, ErrNoActiveModel) {
		t.Errorf("error = %v, want ErrNoActiveModel", err)
	}
}

// TestRetrievePathsCrossoverAndFallback proves the crossover and the index-existence guard: at or below the
// crossover the exact path runs; above it the index path runs only when a valid index exists (filtered →
// iterative, unfiltered → hnsw); above it with NO index the retriever falls back to exact (the frozen
// "index existence is not assumed" rule). It uses a lowered crossover so a few rows exercise both sides.
func TestRetrievePathsCrossoverAndFallback(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	r := &Retriever{crossover: 2}
	query := []float32{1, 0, 0, 0}
	r1 := Filters{Scopes: []string{"run:r1"}}
	r2 := Filters{Scopes: []string{"run:r2"}}
	unfiltered := Filters{IncludeQuarantine: true}

	// m1 and m2 are in BOTH scopes; m3 only in r1. So an r1 query sees 3 rows (above the crossover) while
	// an r2 query sees exactly 2 (at the crossover) — which lets both sides of the crossover be pinned with
	// an index present.
	insertMem(ctx, t, st, proj, "m1", "normal", []string{"run:r1", "run:r2"}, []float32{1, 0, 0, 0})
	insertMem(ctx, t, st, proj, "m2", "normal", []string{"run:r1", "run:r2"}, []float32{0, 1, 0, 0})

	// Two rows (r1 matches both): at the crossover → exact.
	if _, path := retrieve(ctx, t, st, r, proj, query, r1, 10); path != PathExact {
		t.Errorf("2 rows: path = %q, want exact", path)
	}

	// Three rows, still NO index: above the crossover but the retriever must NOT assume an index — exact.
	insertMem(ctx, t, st, proj, "m3", "normal", []string{"run:r1"}, []float32{0.6, 0.8, 0, 0})
	if _, path := retrieve(ctx, t, st, r, proj, query, r1, 10); path != PathExact {
		t.Errorf("3 rows, no index: path = %q, want exact (index existence is not assumed)", path)
	}

	// Build the index. Now the path depends on the candidate count and the filter.
	if err := store.NewPgVectorIndex(st.Pool).EnsureIndex(ctx, proj, testDim); err != nil {
		t.Fatalf("ensure index: %v", err)
	}

	// Above the crossover, filtered → iterative (nearest first).
	res, path := retrieve(ctx, t, st, r, proj, query, r1, 10)
	if path != PathIterative {
		t.Errorf("3 rows, index, filtered: path = %q, want iterative", path)
	}
	if len(res) == 0 || res[0].Content != "m1" {
		t.Errorf("iterative results = %v, want the nearest (m1) first", contents(res))
	}

	// AT the crossover WITH a valid index must still be exact — an r2 query matches exactly 2 rows. This
	// pins the `count <= crossover` boundary (kills a `<` or `==` mutant that would take the index path).
	if _, path := retrieve(ctx, t, st, r, proj, query, r2, 10); path != PathExact {
		t.Errorf("count==crossover with index: path = %q, want exact (r2 matches exactly 2)", path)
	}

	// One clause of the filtered predicate true (scope set but quarantine included) is still filtered →
	// iterative, not plain hnsw: a scope-filtered query must keep iterative_scan or its recall collapses.
	// This pins the `||` in the filtered decision (a `&&` mutant would drop to hnsw here).
	if _, path := retrieve(ctx, t, st, r, proj, query, Filters{Scopes: []string{"run:r1"}, IncludeQuarantine: true}, 10); path != PathIterative {
		t.Errorf("3 rows, index, scoped+include-quarantine: path = %q, want iterative", path)
	}

	// Above the crossover, unfiltered → plain hnsw.
	if _, path := retrieve(ctx, t, st, r, proj, query, unfiltered, 10); path != PathHNSW {
		t.Errorf("3 rows, index, unfiltered: path = %q, want hnsw", path)
	}

	// The filtered index path applies hnsw.iterative_scan=strict_order — read it back in the SAME
	// transaction to pin the SET LOCAL: deleting it, or a relaxed_order swap, would leave a different value.
	if err := st.WithProject(ctx, proj, func(tx pgx.Tx) error {
		if _, _, e := r.Retrieve(ctx, tx, proj, pgvector.NewVector(query), r1, 10); e != nil {
			return e
		}
		var setting string
		if e := tx.QueryRow(ctx, `SELECT current_setting('hnsw.iterative_scan')`).Scan(&setting); e != nil {
			return e
		}
		if setting != "strict_order" {
			t.Errorf("hnsw.iterative_scan = %q after a filtered index retrieve, want strict_order", setting)
		}
		return nil
	}); err != nil {
		t.Fatalf("iterative_scan readback: %v", err)
	}
}

// TestRetrieveIndexIsUsable is the anti-drift guard: with the HNSW index built over enough rows that the
// approximate scan is genuinely cheaper than sorting the whole partition, EXPLAIN of the index-backed
// query must show the HNSW index — a drift between the query's ORDER BY expression and the index's built
// expression would silently fall to a sequential scan (a small dataset can't show this, because the
// planner correctly prefers an exact scan there). It EXPLAINs the unfiltered query so the candidate set is
// the whole partition, the case the HNSW index exists to serve.
func TestRetrieveIndexIsUsable(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)

	// Bulk-insert enough live memories + random dim-4 embeddings that the planner prefers the ANN index
	// over a full sort. One statement keeps it fast.
	if _, err := st.Pool.Exec(ctx, `
		WITH mems AS (
			INSERT INTO memories (project_id, kind, content, scope_keys, trust_tier)
			SELECT $1, 'semantic', 'm' || g, ARRAY['run:r1'], 'normal' FROM generate_series(1, 2000) g
			RETURNING id
		)
		INSERT INTO embeddings (project_id, memory_id, model_id, vec)
		SELECT $1, id, $2, ('[' || random() || ',' || random() || ',' || random() || ',' || random() || ']')::vector
		FROM mems`, proj, testModel); err != nil {
		t.Fatalf("bulk seed: %v", err)
	}
	if err := store.NewPgVectorIndex(st.Pool).EnsureIndex(ctx, proj, testDim); err != nil {
		t.Fatalf("ensure index: %v", err)
	}
	// Refresh planner statistics after the bulk load: without it the planner estimates a near-empty table
	// and always prefers a sort, so the index would never be costed as cheaper regardless of the expression.
	if _, err := st.Pool.Exec(ctx, `ANALYZE memories, embeddings`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	err := st.WithProject(ctx, proj, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = strict_order`); err != nil {
			return err
		}
		// Unfiltered (empty scopes, include quarantine): the whole partition is the candidate set, so the
		// HNSW index is the cheapest way to get the nearest few.
		rows, err := tx.Query(ctx, "EXPLAIN "+indexQuery(testDim),
			proj, testModel, []string{}, true, 10, pgvector.NewVector([]float32{0.5, 0.5, 0.5, 0.5}))
		if err != nil {
			return err
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			plan.WriteString(line)
			plan.WriteByte('\n')
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !strings.Contains(strings.ToLower(plan.String()), "hnsw") {
			t.Errorf("EXPLAIN did not use the HNSW index (ORDER BY / index-expression drift?):\n%s", plan.String())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
}
