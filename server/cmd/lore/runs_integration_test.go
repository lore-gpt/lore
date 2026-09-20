//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

// runRuns executes one `lore runs` subcommand with fresh command state, capturing everything it printed on
// either stream — the table goes to stdout and the guidance to stderr, and an operator sees both.
func runRuns(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := runsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// TestRunsMaintenance covers `lore runs` end to end against a real database: what the operator is told when
// nothing is wrong, what they are told when a run has stopped distilling, and what requeueing one does —
// including the second ask, which must report that a pass was already there rather than claim a new one.
func TestRunsMaintenance(t *testing.T) {
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, paradeDBImage,
		tcpostgres.WithDatabase("lore"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start paradedb container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("store migrations: %v", err)
	}
	t.Setenv("LORE_DATABASE_URL", dsn)

	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := queue.Migrate(ctx, st.Pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	q := db.New(st.Pool)

	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "runs"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	t.Run("a healthy deployment says so instead of printing an empty table", func(t *testing.T) {
		out, err := runRuns(t, "stalled")
		if err != nil {
			t.Fatalf("runs stalled: %v", err)
		}
		if !strings.Contains(out, "no stalled runs") {
			t.Errorf("output = %q, want it to say there are none", out)
		}
	})

	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := q.InsertEvent(ctx, db.InsertEventParams{
			RunID: run.ID, AgentID: "a", Payload: []byte(`{"content":"hello"}`),
		}); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
	runID := uuid.UUID(run.ID.Bytes).String()

	t.Run("a parked run is listed with what the operator needs to act", func(t *testing.T) {
		out, err := runRuns(t, "stalled")
		if err != nil {
			t.Fatalf("runs stalled: %v", err)
		}
		if !strings.Contains(out, runID) {
			t.Errorf("output %q does not name the stalled run %s", out, runID)
		}
		if !strings.Contains(out, "lore runs requeue") {
			t.Error("the listing does not say what to do about it; a report with no next step is why lists get ignored")
		}
	})

	t.Run("requeue queues a pass, and saying it twice does not queue two", func(t *testing.T) {
		first, err := runRuns(t, "requeue", runID)
		if err != nil {
			t.Fatalf("runs requeue: %v", err)
		}
		if !strings.Contains(first, "queued an extraction pass") {
			t.Errorf("first requeue said %q, want it to report the pass it queued", first)
		}

		second, err := runRuns(t, "requeue", runID)
		if err != nil {
			t.Fatalf("second runs requeue: %v", err)
		}
		if !strings.Contains(second, "already has an extraction pass") {
			t.Errorf("second requeue said %q, want it to report that one was already there", second)
		}

		var n int
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM river_job WHERE args->>'run_id' = $1`, runID).Scan(&n); err != nil {
			t.Fatalf("count jobs: %v", err)
		}
		if n != 1 {
			t.Errorf("job rows = %d, want 1", n)
		}
	})

	t.Run("a run that is no longer stalled drops off the list", func(t *testing.T) {
		out, err := runRuns(t, "stalled")
		if err != nil {
			t.Fatalf("runs stalled: %v", err)
		}
		if strings.Contains(out, runID) {
			t.Errorf("output %q still lists a run whose pass is queued", out)
		}
	})

	t.Run("a run with no datable backlog says so instead of printing a nonsense age", func(t *testing.T) {
		other, err := q.InsertRun(ctx, proj.ID)
		if err != nil {
			t.Fatalf("insert run: %v", err)
		}
		if _, err := q.InsertEvent(ctx, db.InsertEventParams{
			RunID: other.ID, AgentID: "a", Payload: []byte(`{"content":"hello"}`),
		}); err != nil {
			t.Fatalf("insert event: %v", err)
		}
		if _, err := st.Pool.Exec(ctx, `DELETE FROM events WHERE run_id = $1`, other.ID); err != nil {
			t.Fatalf("purge events: %v", err)
		}

		out, err := runRuns(t, "stalled")
		if err != nil {
			t.Fatalf("runs stalled: %v", err)
		}
		otherID := uuid.UUID(other.ID.Bytes).String()
		if !strings.Contains(out, otherID) {
			t.Errorf("output %q drops a run that is still behind", out)
		}
		if !strings.Contains(out, "unknown") {
			t.Errorf("output %q does not name the age it cannot fill; a zero instant would print as ~2000 years", out)
		}
	})

	t.Run("an unknown run is named as unknown rather than silently queued", func(t *testing.T) {
		_, err := runRuns(t, "requeue", uuid.New().String())
		if err == nil {
			t.Fatal("requeue of an unknown run = nil error, want one")
		}
		if !strings.Contains(err.Error(), "unknown run") {
			t.Errorf("error = %v, want it to say the run is unknown", err)
		}
	})

	t.Run("a malformed id fails before any database work", func(t *testing.T) {
		_, err := runRuns(t, "requeue", "not-a-uuid")
		if err == nil {
			t.Fatal("requeue of a malformed id = nil error, want one")
		}
		if !strings.Contains(err.Error(), "UUID") {
			t.Errorf("error = %v, want it to name the expected form", err)
		}
	})
}
