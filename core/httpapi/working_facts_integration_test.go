//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/httpapi"
	"github.com/lore-gpt/lore/core/pack"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/retrieval"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/workmem"
)

// packHandlerWithStripe is packHandler plus a working-memory stripe, so a test can drive the write-through
// and the pack against the same server.
func packHandlerWithStripe(st *store.Store, q *queue.Queue, wm workmem.Store) http.Handler {
	packer := pack.New(retrieval.NewHybrid(retrieval.New(), ext.FixtureEmbedder{}))
	return httpapi.New(httpapi.Config{
		Pool: st.Pool, Enqueuer: q, DB: st, Queue: q, Packer: packer, Tenant: st, Version: "test", Workmem: wm,
	}).Handler()
}

// stateBodyFor builds a kind:"state" event body for a run.
func stateBodyFor(runID, entity, predicate, value string) string {
	return fmt.Sprintf(
		`{"run_id":%q,"agent_id":"researcher","payload":{"kind":"state","entity":%q,"predicate":%q,"value":%s}}`,
		runID, entity, predicate, value)
}

// workingFactCount returns how many working facts a run holds.
func workingFactCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM working_facts WHERE run_id = $1`, mustUUID(t, runID)).Scan(&n); err != nil {
		t.Fatalf("count working facts: %v", err)
	}
	return n
}

// readWorkingFact returns the stored row for a subject.
func readWorkingFact(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID, entity, predicate string) (value string, seq int64, agent string) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT value::text, seq, agent_id FROM working_facts
		  WHERE run_id = $1 AND entity = $2 AND predicate = $3`,
		mustUUID(t, runID), entity, predicate).Scan(&value, &seq, &agent); err != nil {
		t.Fatalf("read working fact: %v", err)
	}
	return value, seq, agent
}

// TestStateEventWritesWorkingFactInTheEventTransaction proves the durable half of the working-memory
// write path: a state fact is recorded in the same transaction that appends its event, unconditionally
// — not as a fallback for a sick cache, and not after the commit.
//
// Both properties are load-bearing and both are easy to get wrong in a way nothing else would notice.
// Writing after the commit would leave a window where a 202 has been returned for a fact that is not
// stored; writing only when the cache is unavailable would reproduce the original defect, in which the
// working section existed only for as long as a cache happened to be up.
func TestStateEventWritesWorkingFactInTheEventTransaction(t *testing.T) {
	ctx := context.Background()
	st, q := newStateTestStore(ctx, t)
	projectID, _ := seedProjectRun(ctx, t, st.Pool)
	apiKey, _ := provisionKey(ctx, t, st.Pool, projectID)

	// The fact is recorded whatever the cache is doing. Each mode gets its own run so the counts are
	// unambiguous, and the failing-write case matters most: the stripe reports Healthy, the write
	// through to it fails, and the durable record must still be there.
	t.Run("written_whatever_the_stripe_is_doing", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			store workmem.Store
		}{
			{name: "disabled", store: workmem.NewDisabled()},
			{name: "healthy", store: workmem.NewMemory()},
			{name: "degraded", store: &captureStore{mode: workmem.Degraded}},
			{name: "healthy_but_write_fails", store: &captureStore{mode: workmem.Healthy, err: errBoom}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, run := seedProjectRunIn(ctx, t, st.Pool, projectID)
				handler := httpapi.New(httpapi.Config{
					Pool: st.Pool, Enqueuer: q, DB: st, Queue: q, Version: "test", Workmem: tc.store,
				}).Handler()

				seq := postEvent(t, handler, apiKey, stateBodyFor(run, "auth", "status", `"up"`))

				if got := workingFactCount(ctx, t, st.Pool, run); got != 1 {
					t.Fatalf("working facts with a %s stripe = %d, want 1 — the durable write is not conditional on the cache", tc.name, got)
				}
				value, gotSeq, agent := readWorkingFact(ctx, t, st.Pool, run, "auth", "status")
				if value != `"up"` || gotSeq != seq || agent != "researcher" {
					t.Errorf("stored fact = {%s, seq %d, %s}, want {\"up\", %d, researcher}", value, gotSeq, agent, seq)
				}
			})
		}
	})

	// Atomicity: the fact and the event are one unit. A failure after both have been written must
	// leave neither. This is what a post-commit or separate-transaction write would fail.
	t.Run("rolls_back_with_the_event", func(t *testing.T) {
		_, run := seedProjectRunIn(ctx, t, st.Pool, projectID)
		handler := httpapi.New(httpapi.Config{
			Pool: st.Pool, Enqueuer: failingEnqueuer{}, DB: st, Queue: q, Version: "test",
			Workmem: workmem.NewMemory(),
		}).Handler()

		if rr := do(handler, apiKey, stateBodyFor(run, "auth", "status", `"up"`)); rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %q)", rr.Code, rr.Body.String())
		}
		if got := workingFactCount(ctx, t, st.Pool, run); got != 0 {
			t.Errorf("working facts after a failed enqueue = %d, want 0 — the fact must roll back with the event", got)
		}
		var events int
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM events WHERE run_id = $1`, mustUUID(t, run)).Scan(&events); err != nil {
			t.Fatalf("count events: %v", err)
		}
		if events != 0 {
			t.Errorf("events after a failed enqueue = %d, want 0", events)
		}
	})

	// Atomicity, proved rather than inferred: the fact and its event carry the same transaction id, so ONE
	// transaction wrote both.
	//
	// The rollback sub-test above cannot show this on its own. It drives the failure with a failing enqueue,
	// which happens BEFORE the commit — so a write placed after the commit would never run in that case
	// either, and would leave the same empty table. Both placements pass it. Postgres's per-row xmin is the
	// discriminator that separates them: a post-commit write is a different transaction and carries a
	// different id, however correct its result looks afterwards.
	t.Run("fact_and_event_share_one_transaction", func(t *testing.T) {
		_, run := seedProjectRunIn(ctx, t, st.Pool, projectID)
		handler := httpapi.New(httpapi.Config{
			Pool: st.Pool, Enqueuer: q, DB: st, Queue: q, Version: "test", Workmem: workmem.NewMemory(),
		}).Handler()

		seq := postEvent(t, handler, apiKey, stateBodyFor(run, "auth", "status", `"up"`))

		var sameTx bool
		if err := st.Pool.QueryRow(ctx,
			`SELECT e.xmin = w.xmin
			   FROM events e
			   JOIN working_facts w ON w.project_id = e.project_id AND w.run_id = e.run_id
			  WHERE e.run_id = $1 AND e.seq = $2 AND w.entity = 'auth' AND w.predicate = 'status'`,
			mustUUID(t, run), seq).Scan(&sameTx); err != nil {
			t.Fatalf("compare the event's and the fact's transaction ids: %v", err)
		}
		if !sameTx {
			t.Error("the working fact and its event carry different transaction ids: the fact was written by a " +
				"separate transaction, so a 202 can be returned for a fact that is not stored")
		}
	})

	// A second write to the same subject replaces the first rather than accumulating, and the pack
	// therefore sees one current value per subject.
	t.Run("second_write_replaces_the_subject", func(t *testing.T) {
		_, run := seedProjectRunIn(ctx, t, st.Pool, projectID)
		handler := httpapi.New(httpapi.Config{
			Pool: st.Pool, Enqueuer: q, DB: st, Queue: q, Version: "test", Workmem: workmem.NewDisabled(),
		}).Handler()

		postEvent(t, handler, apiKey, stateBodyFor(run, "auth", "status", `"up"`))
		seq2 := postEvent(t, handler, apiKey, stateBodyFor(run, "auth", "status", `"down"`))

		if got := workingFactCount(ctx, t, st.Pool, run); got != 1 {
			t.Errorf("rows after two writes to one subject = %d, want 1", got)
		}
		if value, seq, _ := readWorkingFact(ctx, t, st.Pool, run, "auth", "status"); value != `"down"` || seq != seq2 {
			t.Errorf("stored fact = (%s, seq %d), want the second write", value, seq)
		}
	})

	// An ordinary event carries no working fact. Dropping the state-event guard would turn every
	// event into one.
	t.Run("ordinary_event_writes_no_fact", func(t *testing.T) {
		_, run := seedProjectRunIn(ctx, t, st.Pool, projectID)
		handler := httpapi.New(httpapi.Config{
			Pool: st.Pool, Enqueuer: q, DB: st, Queue: q, Version: "test", Workmem: workmem.NewMemory(),
		}).Handler()

		postEvent(t, handler, apiKey, fmt.Sprintf(`{"run_id":%q,"agent_id":"a","payload":{"memory":"just a memory"}}`, run))

		if got := workingFactCount(ctx, t, st.Pool, run); got != 0 {
			t.Errorf("working facts for a non-state event = %d, want 0", got)
		}
	})

	// The seq stored is the one the server assigned, not anything the client put in the payload. A
	// client-supplied seq would let a caller pin a stale value past the upsert's guard forever.
	t.Run("seq_comes_from_the_event_not_the_payload", func(t *testing.T) {
		_, run := seedProjectRunIn(ctx, t, st.Pool, projectID)
		handler := httpapi.New(httpapi.Config{
			Pool: st.Pool, Enqueuer: q, DB: st, Queue: q, Version: "test", Workmem: workmem.NewDisabled(),
		}).Handler()

		body := fmt.Sprintf(
			`{"run_id":%q,"agent_id":"researcher","payload":{"kind":"state","entity":"auth","predicate":"status","value":"up","seq":9999}}`,
			run)
		seq := postEvent(t, handler, apiKey, body)

		if _, stored, _ := readWorkingFact(ctx, t, st.Pool, run, "auth", "status"); stored != seq {
			t.Errorf("stored seq = %d, want the server-assigned %d (a payload seq must be ignored)", stored, seq)
		}
	})

	// A key for one project cannot write a fact into another project's run: the request is the same 404 an
	// unknown run gets, and nothing is left behind.
	//
	// Be precise about what this does and does not pin. It does NOT distinguish the upsert's placement
	// relative to the project check — the deferred rollback undoes the row either way, so both orderings
	// leave an empty table. What it pins is the outcome a caller and an operator can observe: the cross-tenant
	// write is refused with no existence oracle, and no row survives it in either project.
	t.Run("cross_tenant_write_leaves_nothing", func(t *testing.T) {
		otherProject, otherRun := seedProjectRun(ctx, t, st.Pool)
		handler := httpapi.New(httpapi.Config{
			Pool: st.Pool, Enqueuer: q, DB: st, Queue: q, Version: "test", Workmem: workmem.NewMemory(),
		}).Handler()

		rr := do(handler, apiKey, stateBodyFor(otherRun, "auth", "status", `"up"`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("cross-tenant status = %d, want 404 (body %q)", rr.Code, rr.Body.String())
		}
		if got := workingFactCount(ctx, t, st.Pool, otherRun); got != 0 {
			t.Errorf("working facts in the other project's run = %d, want 0", got)
		}
		var leftover int
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM working_facts WHERE project_id = $1`, mustUUID(t, otherProject)).Scan(&leftover); err != nil {
			t.Fatalf("count other project's facts: %v", err)
		}
		if leftover != 0 {
			t.Errorf("working facts in the other project = %d, want 0", leftover)
		}
	})
}

// TestPackIgnoresAStaleCacheValue is the regression lock for a whole class of bug, driven end to end
// through the real server: a healthy cache that holds an OLDER value than the durable record must not be
// able to put that older value in a pack.
//
// The scenario is not hypothetical. The extraction worker re-emits a run's state facts into the cache as it
// processes them, and that write is an unguarded overwrite — a delayed pass can put an older value back over
// a newer one. While the pack preferred the cache, that produced a pack that was stale and said nothing about
// it; the durable table underneath held the right answer the whole time. This test recreates exactly that
// interleaving and pins the outcome.
func TestPackIgnoresAStaleCacheValue(t *testing.T) {
	ctx := context.Background()
	st, q := newStateTestStore(ctx, t)
	projectID, runID := seedProjectRun(ctx, t, st.Pool)
	apiKey, _ := provisionKey(ctx, t, st.Pool, projectID)
	setActiveModel(ctx, t, st.Pool, projectID, "fixture-embed-v1@64")

	mem := workmem.NewMemory()
	handler := packHandlerWithStripe(st, q, mem)

	// Two writes to one subject. Both land in the durable table and, via the write-through, in the cache.
	seq1 := postEvent(t, handler, apiKey, stateBodyFor(runID, "auth", "status", `"first"`))
	postEvent(t, handler, apiKey, stateBodyFor(runID, "auth", "status", `"second"`))

	// The delayed extraction pass: it re-emits the OLDER event into the cache, over the newer value.
	key := workmem.Key{ProjectID: projectID, RunID: runID, Entity: "auth", Predicate: "status"}
	if err := mem.Set(ctx, key, workmem.Value{Value: []byte(`"first"`), Seq: seq1, Agent: "researcher"}); err != nil {
		t.Fatalf("simulate the delayed pass: %v", err)
	}
	// Precondition: the cache really is stale now. Without this the assertion below could pass for the
	// wrong reason — a cache that happened to hold the right value proves nothing.
	if v, ok, err := mem.Get(ctx, key); err != nil || !ok || string(v.Value) != `"first"` {
		t.Fatalf("cache should hold the stale value: v=%s ok=%v err=%v", v.Value, ok, err)
	}

	rr := doPack(handler, apiKey, fmt.Sprintf(`{"run_id":%q,"query":"status"}`, runID))
	if rr.Code != http.StatusOK {
		t.Fatalf("pack status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	// Both assertions match the WORKING-SECTION render (`entity.predicate = value`), not the bare value.
	// The raw tail carries both events' payloads verbatim, so a substring check for "second" alone would
	// pass even if the working section were empty — the very failure this test exists to catch. The values
	// appear JSON-escaped because the pack text is a field of the JSON response body.
	body := rr.Body.String()
	if !strings.Contains(body, `auth.status = \"second\"`) {
		t.Errorf("working section must carry the newest value from the durable record:\n%s", body)
	}
	if strings.Contains(body, `auth.status = \"first\"`) {
		t.Errorf("working section served the stale cached value:\n%s", body)
	}
}
