//go:build integration

package retrieval

import (
	"context"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/lore-gpt/lore/core/store"
)

// seedScaleTenant bulk-loads a tenant to a target scale for the read-path plan guards: nLive in-scope live
// memories plus nExpired expired ones (valid_to set, so the live-row predicate actually filters something),
// each with a random dim-4 embedding. It is the parametric seed a later load harness grows from — raise the
// counts, keep the shape. Direct SQL (not the write path) so a large partition materialises in one round-trip.
func seedScaleTenant(ctx context.Context, t *testing.T, st *store.Store, projectID pgtype.UUID, model string, nLive, nExpired int) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx, `
		WITH live AS (
			INSERT INTO memories (project_id, kind, content, scope_keys, trust_tier)
			SELECT $1, 'semantic', 'memory ' || g || ' about auth deploy search cache', ARRAY['run:r1'],
			       CASE WHEN g % 500 = 0 THEN 'quarantine' ELSE 'normal' END
			FROM generate_series(1, $3::int) g
			RETURNING id
		)
		INSERT INTO embeddings (project_id, memory_id, model_id, vec)
		SELECT $1, id, $2, ('[' || random() || ',' || random() || ',' || random() || ',' || random() || ']')::vector
		FROM live`, projectID, model, nLive); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	if nExpired > 0 {
		if _, err := st.Pool.Exec(ctx, `
			WITH expired AS (
				INSERT INTO memories (project_id, kind, content, scope_keys, trust_tier, valid_to)
				SELECT $1, 'semantic', 'expired ' || g || ' about auth deploy search cache', ARRAY['run:r1'], 'normal', now()
				FROM generate_series(1, $3::int) g
				RETURNING id
			)
			INSERT INTO embeddings (project_id, memory_id, model_id, vec)
			SELECT $1, id, $2, ('[' || random() || ',' || random() || ',' || random() || ',' || random() || ']')::vector
			FROM expired`, projectID, model, nExpired); err != nil {
			t.Fatalf("seed expired: %v", err)
		}
	}
}

// seedRunEvents inserts nEvents uncovered events on a fresh run of the project, returning the run id. It is
// the raw-tail side of the parametric seed.
func seedRunEvents(ctx context.Context, t *testing.T, st *store.Store, projectID pgtype.UUID, nEvents int) string {
	t.Helper()
	var runID string
	if err := st.Pool.QueryRow(ctx, `INSERT INTO runs (project_id) VALUES ($1) RETURNING id`, projectID).Scan(&runID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO events (project_id, run_id, agent_id, payload, seq)
		SELECT $1, $2, 'a', '{"note":"e"}'::jsonb, g FROM generate_series(1, $3::int) g`, projectID, runID, nEvents); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	// Leave the run in the state the write path would: last_seq at the highest event seq, so a pack-level
	// harness driving Build against this seed can validate min_seq against last_seq rather than seeing 0.
	if _, err := st.Pool.Exec(ctx, `UPDATE runs SET last_seq = $2 WHERE id = $1`, runID, nEvents); err != nil {
		t.Fatalf("advance run last_seq: %v", err)
	}
	return runID
}

var execTimeRe = regexp.MustCompile(`Execution Time: ([0-9.]+) ms`)

// TestReadPathPartitionAndRawTailPlans is the perf-hardening guard for the plan classes NOT already pinned
// elsewhere (dense HNSW usage → TestRetrieveIndexIsUsable; full-text GIN usage → TestLexicalLegPropagation-
// AndIsolation). At a realistic scale (10k live + 10k expired memories in one tenant, a second populated
// tenant, a run with many uncovered events, the HNSW built by the PROD EnsureIndex) it EXPLAINs each query
// and pins: the dense and partition-scanned legs stay inside the tenant's OWN partition (no cross-tenant
// Append — the multi-tenant p95 killer), and the raw-tail windows ride the (run_id, seq) index rather than a
// sequential scan of the whole events table. It also logs a one-time rough DB-only timing (a report line,
// not a gate: the numeric p95 target is measured against a real embedding provider by the load harness, and
// the embedding call — the dominant end-to-end cost — is a fixture here).
func TestReadPathPartitionAndRawTailPlans(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	big := seedProject(ctx, t, st, testModel)
	small := seedProject(ctx, t, st, testModel)

	seedScaleTenant(ctx, t, st, big, testModel, 10000, 10000)
	seedScaleTenant(ctx, t, st, small, testModel, 200, 0) // a second populated partition, so pruning is observable
	runID := seedRunEvents(ctx, t, st, big, 10000)

	if err := store.NewPgVectorIndex(st.Pool).EnsureIndex(ctx, big, testDim); err != nil {
		t.Fatalf("ensure index: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `ANALYZE memories, embeddings, events`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	bigSuffix := hex.EncodeToString(big.Bytes[:])
	smallSuffix := hex.EncodeToString(small.Bytes[:])

	// explain runs EXPLAIN (ANALYZE) for sql inside a tenant transaction (RLS on, as in production) and
	// returns the lower-cased plan text and the reported execution time in ms.
	explain := func(sql string, args ...any) (string, float64) {
		t.Helper()
		var plan string
		var ms float64
		if err := st.WithProject(ctx, big, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			var b strings.Builder
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					return err
				}
				b.WriteString(line)
				b.WriteByte('\n')
			}
			if err := rows.Err(); err != nil {
				return err
			}
			plan = b.String()
			if m := execTimeRe.FindStringSubmatch(plan); m != nil {
				ms, _ = strconv.ParseFloat(m[1], 64)
			}
			return nil
		}); err != nil {
			t.Fatalf("explain: %v", err)
		}
		return strings.ToLower(plan), ms
	}

	// assertTenantLocal fails if the plan scans anything but the big tenant's own memories partition — the
	// other tenant's partition or a cross-partition Append means project_id pruning drifted.
	assertTenantLocal := func(label, plan string) {
		if !strings.Contains(plan, "memories_p_"+bigSuffix) {
			t.Errorf("%s did not scan the tenant partition memories_p_%s:\n%s", label, bigSuffix, plan)
		}
		if strings.Contains(plan, "memories_p_"+smallSuffix) {
			t.Errorf("%s touched the OTHER tenant's partition memories_p_%s (pruning drifted):\n%s", label, smallSuffix, plan)
		}
		if strings.Contains(plan, "append") {
			t.Errorf("%s scanned more than one partition (Append present, pruning drifted):\n%s", label, plan)
		}
	}

	// assertEventsIndexScan fails if the plan sequentially scans the (unpartitioned, shared) events table
	// instead of riding an events index. It checks the property that matters — index-backed, not a whole-table
	// scan that grows with total events across all runs — rather than a specific index name, so a planner
	// switch between equally-valid events indexes does not false-red while a true fall to a seq scan fails.
	assertEventsIndexScan := func(label, plan string) {
		if strings.Contains(plan, "seq scan on events") {
			t.Errorf("%s fell to a sequential scan of the shared events table:\n%s", label, plan)
		}
		if !strings.Contains(plan, "index scan") {
			t.Errorf("%s did not ride an events index:\n%s", label, plan)
		}
	}

	scopes := []string{"run:r1"}
	empty := []string{}
	qv := pgvector.NewVector([]float32{0.5, 0.5, 0.5, 0.5})
	report := map[string]float64{}

	// Dense index path: at 10k the planner takes the HNSW index; it must stay partition-local.
	densePlan, denseMs := explain(indexQuery(testDim), big, testModel, empty, true, 10, qv)
	report["dense"] = denseMs
	if !strings.Contains(densePlan, "hnsw") {
		t.Errorf("dense leg did not use the HNSW index at 10k (expression drift?):\n%s", densePlan)
	}
	assertTenantLocal("dense", densePlan)

	// Count (project-wide) — partition-scanned by design; must stay tenant-local. The SQL mirrors the plan
	// shape of CountRetrievalCandidates (core/store/queries/retrieval.sql); keep the two in lockstep.
	countPlan, countMs := explain(`SELECT count(*) FROM (
		SELECT 1 FROM memories m JOIN embeddings e ON e.project_id=m.project_id AND e.memory_id=m.id
		WHERE m.project_id=$1 AND e.project_id=$1 AND e.model_id=$2
		  AND m.superseded_by IS NULL AND m.valid_to IS NULL
		  AND (cardinality($3::text[])=0 OR m.scope_keys && $3::text[])
		  AND ($4::bool OR m.trust_tier <> 'quarantine')
		LIMIT $5::int) t`, big, testModel, empty, false, 8001)
	report["count"] = countMs
	assertTenantLocal("count", countPlan)

	// Exact (scoped) — the small-candidate crossover path; partition-scanned, must stay tenant-local. The SQL
	// mirrors the plan shape of RetrieveExact (core/store/queries/retrieval.sql); keep the two in lockstep.
	exactPlan, exactMs := explain(`SELECT m.id, m.content, m.kind, (e.vec <=> $6::vector)::float8 AS distance
		FROM memories m JOIN embeddings e ON e.project_id=m.project_id AND e.memory_id=m.id
		WHERE m.project_id=$1 AND e.project_id=$1 AND e.model_id=$2
		  AND m.superseded_by IS NULL AND m.valid_to IS NULL
		  AND (cardinality($3::text[])=0 OR m.scope_keys && $3::text[])
		  AND ($4::bool OR m.trust_tier <> 'quarantine')
		ORDER BY distance ASC LIMIT $5::int`, big, testModel, scopes, false, 10, qv)
	report["exact"] = exactMs
	assertTenantLocal("exact", exactPlan)

	// Raw-tail guaranteed window — the read-your-writes guarantee. A fall to a sequential scan of the shared
	// events table is the p95 killer at many-runs scale; an events index must serve the range. The SQL mirrors
	// the plan shape of PackRawTailGuaranteed (core/store/queries/pack.sql); keep the two in lockstep.
	rtG, rtgMs := explain(`SELECT id, agent_id, payload, created_at, seq FROM events
		WHERE project_id=$1 AND run_id=$2 AND seq > $3 AND seq <= $4 ORDER BY seq`, big, runID, int64(0), int64(9000))
	report["rawtail_guaranteed"] = rtgMs
	assertEventsIndexScan("raw-tail guaranteed window", rtG)

	// Raw-tail beyond window — newest-first, capped. Mirrors PackRawTailBeyond (pack.sql); keep in lockstep.
	rtB, rtbMs := explain(`SELECT id, agent_id, payload, created_at, seq FROM events
		WHERE project_id=$1 AND run_id=$2 AND seq > GREATEST($3::bigint,$4::bigint) ORDER BY seq DESC LIMIT $5::int`,
		big, runID, int64(0), int64(0), 50)
	report["rawtail_beyond"] = rtbMs
	assertEventsIndexScan("raw-tail beyond window", rtB)

	// Freshness — runs on every pack build. It reads the oldest uncovered event's created_at via ORDER BY seq
	// LIMIT 1 (the min-seq uncovered event is also the min-created_at one), so it must ride an events index
	// rather than aggregate min(created_at) over the whole uncovered set, which the planner can serve with a
	// full scan of the shared events table. Mirrors PackFreshness (pack.sql); keep in lockstep.
	fr, frMs := explain(`SELECT coalesce(extract(epoch FROM now() - (
			SELECT events.created_at FROM events
			WHERE events.project_id=$1 AND events.run_id=$2 AND events.seq > $3
			ORDER BY events.seq LIMIT 1)) * 1000, 0)::bigint`, big, runID, int64(0))
	report["freshness"] = frMs
	assertEventsIndexScan("freshness", fr)

	// One-time rough DB-only timing (fixture embed, dim 4). NOT a gate — the numeric p95 target is measured
	// against a real embedding provider by the separate load harness. Two caveats on reading these figures:
	// (1) the embedding call — the dominant end-to-end cost — is a fixture here, so this is a DB-only floor;
	// (2) the printed dense/exact millis are a DIM-4 floor: the cosine distance work scales ~linearly with the
	// model's vector width (a real 768-1536 dim model does far more per-comparison arithmetic), so the plan
	// CLASS is what is asserted, not these absolute numbers. Logged so a gross DB regression is visible now.
	t.Logf("read-path DB-only timings (ms) @10k live +10k expired, fixture dim %d: dense=%.2f count=%.2f exact=%.2f rawtail_guaranteed=%.2f rawtail_beyond=%.2f freshness=%.2f",
		testDim, report["dense"], report["count"], report["exact"], report["rawtail_guaranteed"], report["rawtail_beyond"], report["freshness"])
}
