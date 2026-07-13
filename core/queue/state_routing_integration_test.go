//go:build integration

package queue_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
)

const stateFactPayload = `{"kind":"state","entity":"auth","predicate":"status","value":"up"}`

// TestExtractRunWorkerRoutesStateFactsIntegration proves the worker's state routing against real Postgres:
// with a healthy stripe a kind:"state" event lands in the hot lane and writes NO durable claim; with the
// stripe disabled the SAME event is preserved as a durable, queryable claim (the paired assertion). Either
// way the checkpoint advances and the model is never invoked for a state event. It drives Work directly so
// the pass is deterministic (River's scheduling is proven elsewhere).
func TestExtractRunWorkerRoutesStateFactsIntegration(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	q := db.New(st.Pool)

	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := store.CreateProjectPartitions(ctx, st.Pool, proj.ID); err != nil {
		t.Fatalf("create partitions: %v", err)
	}
	projectID := uuid.UUID(proj.ID.Bytes).String()

	// seedStateRun creates a run and appends one state event, returning its ids.
	seedStateRun := func() (string, pgtype.UUID) {
		run, err := q.InsertRun(ctx, proj.ID)
		if err != nil {
			t.Fatalf("insert run: %v", err)
		}
		if _, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "planner", Payload: []byte(stateFactPayload)}); err != nil {
			t.Fatalf("insert event: %v", err)
		}
		return uuid.UUID(run.ID.Bytes).String(), run.ID
	}

	// runWork drives one synchronous pass for a run with the given store injected.
	runWork := func(runID string, wm workmem.Store) {
		worker := jobs.NewExtractRunWorker(db.New(st.Pool), &neverExtractor{t: t}, jobs.NewPGPersister(st, ext.LWW{}),
			jobs.Debounce{IdleWindow: 0, MaxEvents: 1}, jobs.WithWorkmemStore(wm))
		job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: projectID, RunID: runID}}
		if err := worker.Work(ctx, job); err != nil {
			t.Fatalf("Work: %v", err)
		}
	}

	claimCount := func(runPg pgtype.UUID) int64 {
		var n int64
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM claims c JOIN events ev ON ev.id = c.source_event_id WHERE c.project_id = $1 AND ev.run_id = $2`,
			proj.ID, runPg).Scan(&n); err != nil {
			t.Fatalf("count claims: %v", err)
		}
		return n
	}
	coveredSeq := func(runPg pgtype.UUID) int64 {
		var n int64
		if err := st.Pool.QueryRow(ctx, `SELECT covered_seq FROM runs WHERE id = $1`, runPg).Scan(&n); err != nil {
			t.Fatalf("read covered_seq: %v", err)
		}
		return n
	}

	// --- Healthy stripe: the fact goes to the hot lane; no durable claim. ---
	t.Run("healthy stripe takes the hot fact, no durable claim", func(t *testing.T) {
		runID, runPg := seedStateRun()
		mem := workmem.NewMemory()
		runWork(runID, mem)

		v, ok, err := mem.Get(ctx, workmem.Key{ProjectID: projectID, RunID: runID, Entity: "auth", Predicate: "status"})
		if err != nil || !ok {
			t.Fatalf("hot fact absent after a healthy pass: ok=%v err=%v", ok, err)
		}
		if string(v.Value) != `"up"` || v.Seq != 1 || v.Agent != "planner" {
			t.Errorf("hot fact = {%s, seq %d, %s}, want {\"up\", 1, planner}", v.Value, v.Seq, v.Agent)
		}
		if n := claimCount(runPg); n != 0 {
			t.Errorf("durable claims after a healthy pass = %d, want 0 (the hot lane owns the fact)", n)
		}
		if c := coveredSeq(runPg); c != 1 {
			t.Errorf("covered_seq = %d, want 1 (the state event is consumed)", c)
		}
	})

	// --- Disabled stripe: the same fact is preserved as a durable, queryable claim. ---
	t.Run("disabled stripe preserves the fact as a durable claim", func(t *testing.T) {
		runID, runPg := seedStateRun()
		runWork(runID, workmem.NewDisabled())

		var name, predicate, value string
		if err := st.Pool.QueryRow(ctx, `
			SELECT e.name, c.predicate, c.value::text
			FROM claims c
			JOIN entities e ON e.id = c.entity_id AND e.project_id = c.project_id
			JOIN events ev ON ev.id = c.source_event_id
			WHERE c.project_id = $1 AND ev.run_id = $2 AND c.superseded_by IS NULL`,
			proj.ID, runPg).Scan(&name, &predicate, &value); err != nil {
			t.Fatalf("read the durable state claim: %v", err)
		}
		if name != "auth" || predicate != "status" || value != `"up"` {
			t.Errorf("state claim = {%q,%q,%s}, want {auth,status,\"up\"}", name, predicate, value)
		}
		if n := claimCount(runPg); n != 1 {
			t.Errorf("durable claims after a disabled pass = %d, want exactly 1", n)
		}
		if c := coveredSeq(runPg); c != 1 {
			t.Errorf("covered_seq = %d, want 1", c)
		}

		// The checkpoint gates reprocessing: the first pass advanced covered_seq past the state event, so a
		// re-run finds nothing past the checkpoint, never re-enters routing, and adds no second claim. (This
		// asserts the checkpoint early-return, not an idempotency property of routing itself — the CAS on the
		// checkpoint under concurrent double-delivery is proven in the persister's own tests.)
		runWork(runID, workmem.NewDisabled())
		if n := claimCount(runPg); n != 1 {
			t.Errorf("durable claims after re-running a consumed run = %d, want 1 (checkpoint gates reprocessing)", n)
		}
	})
}

// neverExtractor fails the test if the extractor is invoked — a state-only window must never reach the model.
type neverExtractor struct{ t *testing.T }

func (n *neverExtractor) Extract(context.Context, ext.ExtractInput) (ext.ExtractResult, error) {
	n.t.Error("extractor was invoked for a state-only window; state events must never be distilled")
	return ext.ExtractResult{}, nil
}
