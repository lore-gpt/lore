//go:build integration

package retrieval

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/store"
)

// stubEmbedder is a controllable Embedder for hybrid tests: it maps a known query text to a chosen vector
// (so dense ordering is deterministic), reports a configurable model id (to exercise the model-match
// guard), can delay (to exercise the dense-leg partial-result budget), and can fail (to exercise the
// late-error path). It honours context cancellation so a test can reclaim a slow embed goroutine.
type stubEmbedder struct {
	vecs  map[string][]float32
	dim   int
	model string
	delay time.Duration
	err   error
}

func (s stubEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := s.vecs[t]
		if !ok {
			// A basis vector, never the zero vector (whose cosine distance is undefined).
			v = make([]float32, s.dim)
			if s.dim > 0 {
				v[0] = 1
			}
		}
		out[i] = v
	}
	return out, nil
}

func (s stubEmbedder) Dim() int        { return s.dim }
func (s stubEmbedder) ModelID() string { return s.model }

// recordHandler is a slog.Handler that forwards each record's message to a channel, so a test can assert a
// specific log line fired (e.g. the drain goroutine's late-embed warning).
type recordHandler struct{ ch chan string }

func (recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordHandler) Handle(_ context.Context, r slog.Record) error {
	select {
	case h.ch <- r.Message:
	default:
	}
	return nil
}
func (h recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordHandler) WithGroup(string) slog.Handler      { return h }

// fakeLeg is a test leg that surfaces one chosen candidate, to prove a non-stub leg's output actually
// reaches fusion (the entity stub proves the neutral case; this proves the wired case).
type fakeLeg struct{ c candidate }

func (fakeLeg) name() string { return "fake" }
func (f fakeLeg) retrieve(context.Context, pgx.Tx, pgtype.UUID, queryInput, Filters, int) ([]candidate, error) {
	return []candidate{f.c}, nil
}

// droppingReranker violates the Reranker contract by returning fewer results than it was given.
type droppingReranker struct{}

func (droppingReranker) Rerank(_ context.Context, _ string, r []HybridResult) ([]HybridResult, error) {
	if len(r) == 0 {
		return r, nil
	}
	return r[:len(r)-1], nil
}

func hcontents(results []HybridResult) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.Content
	}
	return out
}

func statByName(stats []LegStat, name string) (LegStat, bool) {
	for _, s := range stats {
		if s.Name == name {
			return s, true
		}
	}
	return LegStat{}, false
}

// runHybrid executes a hybrid retrieval inside a tenant transaction and fatals on error.
func runHybrid(ctx context.Context, t *testing.T, st *store.Store, h *Hybrid, projectID pgtype.UUID, query string, filters Filters, limit int) ([]HybridResult, []LegStat) {
	t.Helper()
	var res []HybridResult
	var stats []LegStat
	err := st.WithProject(ctx, projectID, func(tx pgx.Tx) error {
		var e error
		res, stats, e = h.Retrieve(ctx, tx, projectID, query, filters, limit)
		return e
	})
	if err != nil {
		t.Fatalf("hybrid retrieve: %v", err)
	}
	return res, stats
}

// TestHybridFusesDenseAndLexical is the end-to-end fusion proof. Three memories are arranged so one is
// strong on dense only, one on lexical only, and one on both; reciprocal rank fusion must rank the two-leg
// memory above the single-leg ones. It also proves: the registered entity leg is a neutral stub (fusion is
// identical with it removed), an empty query makes the lexical leg contribute nothing, and a lexical match
// only surfaces memories that actually contain the term.
func TestHybridFusesDenseAndLexical(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)

	// query text "auth" embeds (via the stub) to [1,0,0,0]; dense distance is 1 - cosine to that.
	both := insertMem(ctx, t, st, proj, "auth token rotation", "normal", []string{"run:r1"}, []float32{0.7, 0.7, 0, 0})  // dense mid, lexical hit
	denseOnly := insertMem(ctx, t, st, proj, "session cache warms", "normal", []string{"run:r1"}, []float32{1, 0, 0, 0}) // dense top, lexical miss
	lexOnly := insertMem(ctx, t, st, proj, "auth auth auth login", "normal", []string{"run:r1"}, []float32{0, 1, 0, 0})  // dense far, lexical hit

	emb := stubEmbedder{vecs: map[string][]float32{"auth": {1, 0, 0, 0}}, dim: testDim, model: testModel}
	h := NewHybrid(New(), emb)

	res, stats := runHybrid(ctx, t, st, h, proj, "auth", Filters{Scopes: []string{"run:r1"}}, 10)

	// The dense-only memory (found by a single leg at its top rank) must sink below both memories the two
	// legs agree on.
	posBoth, posLex, posDense := indexOf(res, both), indexOf(res, lexOnly), indexOf(res, denseOnly)
	if posBoth < 0 || posLex < 0 || posDense < 0 {
		t.Fatalf("missing results: both=%d lexOnly=%d denseOnly=%d in %v", posBoth, posLex, posDense, hcontents(res))
	}
	if posDense < posBoth || posDense < posLex {
		t.Errorf("single-leg dense doc at %d should rank below two-leg docs (both=%d, lexOnly=%d): %v", posDense, posBoth, posLex, hcontents(res))
	}

	// The entity leg is a registered but neutral stub.
	if es, ok := statByName(stats, "entity"); !ok || es.Status != statusStub || es.Count != 0 {
		t.Errorf("entity leg stat = %+v, want {stub, 0}", es)
	}
	// Removing the stub leg entirely must not change fusion.
	control := NewHybrid(New(), emb)
	control.legs = []leg{lexicalLeg{}}
	resControl, _ := runHybrid(ctx, t, st, control, proj, "auth", Filters{Scopes: []string{"run:r1"}}, 10)
	if !equalStrings(hcontents(res), hcontents(resControl)) {
		t.Errorf("entity stub changed fusion: with=%v without=%v", hcontents(res), hcontents(resControl))
	}

	// Forward wiring: a leg that DOES return a candidate must reach fusion. Swapping in a leg that surfaces
	// an id no other leg produces, and seeing it appear, proves legs are actually fused (not ignored) — the
	// complement to the stub-neutrality check above.
	fakeID := mkID(200)
	wired := NewHybrid(New(), emb)
	wired.legs = []leg{lexicalLeg{}, fakeLeg{c: candidate{id: fakeID, content: "FAKE", kind: "semantic"}}}
	resWired, _ := runHybrid(ctx, t, st, wired, proj, "auth", Filters{Scopes: []string{"run:r1"}}, 10)
	if indexOf(resWired, fakeID) < 0 {
		t.Errorf("a non-stub leg's candidate did not reach fusion (legs ignored?): %v", hcontents(resWired))
	}

	// Empty query: the lexical leg matches nothing (an empty tsquery), so it contributes no candidates AND
	// the fused order must equal a dense-only hybrid's — zero lexical candidates is equivalent to the lexical
	// leg not running.
	denseHybrid := NewHybrid(New(), emb)
	denseHybrid.legs = nil // only the (separately orchestrated) dense leg runs
	resEmptyFull, emptyStats := runHybrid(ctx, t, st, h, proj, "   ", Filters{Scopes: []string{"run:r1"}}, 10)
	resEmptyDense, _ := runHybrid(ctx, t, st, denseHybrid, proj, "   ", Filters{Scopes: []string{"run:r1"}}, 10)
	if ls, ok := statByName(emptyStats, "lexical"); !ok || ls.Count != 0 {
		t.Errorf("empty-query lexical stat = %+v, want count 0", ls)
	}
	if !equalStrings(hcontents(resEmptyFull), hcontents(resEmptyDense)) {
		t.Errorf("empty query: full-hybrid order %v != dense-only %v (lexical zero-contribution not neutral)", hcontents(resEmptyFull), hcontents(resEmptyDense))
	}

	// A reranker that changes the result count violates the contract and must fail loud rather than silently
	// corrupt the head/tail boundary.
	bad := NewHybrid(New(), emb, WithReranker(droppingReranker{}))
	badErr := st.WithProject(ctx, proj, func(tx pgx.Tx) error {
		_, _, e := bad.Retrieve(ctx, tx, proj, "auth", Filters{Scopes: []string{"run:r1"}}, 10)
		return e
	})
	if badErr == nil || !strings.Contains(badErr.Error(), "reranker must reorder") {
		t.Errorf("dropping reranker: err = %v, want a reorder-contract error", badErr)
	}
}

// TestHybridPartialTimeoutDropsDense proves the partial-result budget: when the query embedding takes longer
// than the budget, the dense leg is dropped and the read proceeds on the legs that finished. A memory that
// only the dense leg would have surfaced disappears, and the dense leg is reported as timed out — the read
// degrades, never blocks.
func TestHybridPartialTimeoutDropsDense(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // reclaim the slow embed goroutine when the test ends

	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)

	insertMem(ctx, t, st, proj, "auth token rotation", "normal", []string{"run:r1"}, []float32{0, 1, 0, 0})             // lexical hit
	denseMem := insertMem(ctx, t, st, proj, "session cache warms", "normal", []string{"run:r1"}, []float32{1, 0, 0, 0}) // dense-only

	// Embed resolves well past the budget AND ultimately fails — so the dense leg is dropped, and the late
	// failure must still be surfaced to logs rather than silently swallowed by the timeout path.
	rec := recordHandler{ch: make(chan string, 32)}
	emb := stubEmbedder{vecs: map[string][]float32{"auth": {1, 0, 0, 0}}, dim: testDim, model: testModel, delay: 150 * time.Millisecond, err: errors.New("provider unavailable")}
	h := NewHybrid(New(), emb, WithPartialTimeout(30*time.Millisecond), WithLogger(slog.New(rec)))

	start := time.Now()
	res, stats := runHybrid(ctx, t, st, h, proj, "auth", Filters{Scopes: []string{"run:r1"}}, 10)
	if elapsed := time.Since(start); elapsed > 120*time.Millisecond {
		t.Errorf("retrieval took %v, expected to return near the 30ms budget (dense dropped, not awaited)", elapsed)
	}

	ds, ok := statByName(stats, "dense")
	if !ok || ds.Status != statusTimeout {
		t.Errorf("dense leg stat = %+v, want status %q", ds, statusTimeout)
	}
	// The dense-only memory must be absent — only the lexical leg contributed.
	if indexOf(res, denseMem) >= 0 {
		t.Errorf("dense-only memory present after dense timeout: %v", hcontents(res))
	}
	if len(res) == 0 {
		t.Errorf("expected the lexical leg to still return results after dense timeout")
	}

	// The late embedding failure must be logged by the drain goroutine (observability of a degrading provider).
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-rec.ch:
			if strings.Contains(msg, "embedding failed after the partial-result budget") {
				return
			}
		case <-deadline:
			t.Fatalf("late embed failure was not logged")
		}
	}
}

// TestHybridModelGuards proves the two loud configuration errors: a project with no active embedding model
// is ErrNoActiveModel (never a silent empty read), and an embedder whose model does not match the project's
// active model is ErrModelMismatch (never a query vector silently compared in the wrong space).
func TestHybridModelGuards(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	emb := stubEmbedder{vecs: map[string][]float32{"q": {1, 0, 0, 0}}, dim: testDim, model: testModel}

	// No active model.
	noModel := seedProject(ctx, t, st, "")
	err := st.WithProject(ctx, noModel, func(tx pgx.Tx) error {
		_, _, e := NewHybrid(New(), emb).Retrieve(ctx, tx, noModel, "q", Filters{}, 10)
		return e
	})
	if !errors.Is(err, ErrNoActiveModel) {
		t.Errorf("no active model: err = %v, want ErrNoActiveModel", err)
	}

	// Active model set, but the embedder is a different model.
	mismatch := seedProject(ctx, t, st, "some-other-model")
	err = st.WithProject(ctx, mismatch, func(tx pgx.Tx) error {
		_, _, e := NewHybrid(New(), emb).Retrieve(ctx, tx, mismatch, "q", Filters{}, 10)
		return e
	})
	if !errors.Is(err, ErrModelMismatch) {
		t.Errorf("model mismatch: err = %v, want ErrModelMismatch", err)
	}
}

// TestLexicalLegPropagationAndIsolation proves the full-text substrate. The expression GIN index built on
// the partitioned parent propagates to every project's partition (so a fresh tenant is searchable with no
// per-partition build); a lexical query is strictly tenant-isolated; and — with the index made the only
// option — the query's tsvector predicate actually uses the index (its expression matches the index's, so
// no silent sequential-scan drift).
func TestLexicalLegPropagationAndIsolation(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	projA := seedProject(ctx, t, st, testModel)
	projB := seedProject(ctx, t, st, testModel)

	// The parent's fts index must have propagated to BOTH partitions.
	var childFTS int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE tablename LIKE 'memories_p_%' AND indexdef ILIKE '%to_tsvector%'`).Scan(&childFTS); err != nil {
		t.Fatalf("count child fts indexes: %v", err)
	}
	if childFTS != 2 {
		t.Errorf("child fts indexes = %d, want 2 (one per project partition)", childFTS)
	}

	aID := insertMem(ctx, t, st, projA, "alpha authentication flow", "normal", []string{"run:r1"}, []float32{1, 0, 0, 0})
	insertMem(ctx, t, st, projB, "bravo authentication flow", "normal", []string{"run:r1"}, []float32{1, 0, 0, 0})

	emb := stubEmbedder{vecs: map[string][]float32{"authentication": {1, 0, 0, 0}}, dim: testDim, model: testModel}
	h := NewHybrid(New(), emb)

	// Tenant A's query sees only tenant A's memory (rank sanity: the term matches; isolation: B is invisible).
	resA, _ := runHybrid(ctx, t, st, h, projA, "authentication", Filters{}, 10)
	if got := hcontents(resA); len(got) != 1 || got[0] != "alpha authentication flow" {
		t.Errorf("tenant A lexical/dense = %v, want only [alpha authentication flow]", got)
	}
	if indexOf(resA, aID) < 0 {
		t.Errorf("tenant A result missing its own memory")
	}

	// Anti-drift: bulk-load a selective term into project A, ANALYZE, and confirm the fts predicate is served
	// by the index (a bitmap index scan) rather than a sequential scan.
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO memories (project_id, kind, content, scope_keys, trust_tier)
		SELECT $1, 'semantic', 'filler document number ' || g, ARRAY['run:r1'], 'normal' FROM generate_series(1, 3000) g`, projA); err != nil {
		t.Fatalf("bulk seed: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO memories (project_id, kind, content, scope_keys, trust_tier) VALUES ($1,'semantic','uniquezebraterm here',ARRAY['run:r1'],'normal')`, projA); err != nil {
		t.Fatalf("insert selective row: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `ANALYZE memories`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	err := st.WithProject(ctx, projA, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `EXPLAIN
			SELECT m.id FROM memories m
			WHERE m.project_id = $1
			  AND to_tsvector('english', m.content) @@ websearch_to_tsquery('english', 'uniquezebraterm')`, projA)
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
		if !strings.Contains(strings.ToLower(plan.String()), "bitmap index scan") {
			t.Errorf("fts predicate did not use the index (expression drift?):\n%s", plan.String())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
}
