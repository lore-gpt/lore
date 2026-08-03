//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pressly/goose/v3"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/store/migrations"
)

// TestMigration0019WorkingFacts proves the durable working-memory table against a real ParadeDB:
// the upsert collapses a subject to one row, the seq guard refuses to go backwards, the key is
// RUN-scoped rather than project-scoped, the byte-length and seq constraints hold, deleting a run
// or a project takes the facts with it, and the tenant policy scopes reads to the session project.
//
// The run-scoping case is the one worth reading twice. The neighbouring claims table keys a
// same-looking subject as (project_id, entity_id, predicate); reusing that shape here would let one
// run's write hide another run's own fact, which is precisely why a claims-backed working section
// was rejected. Nothing but this test stands between that mistake and a green build.
func TestMigration0019WorkingFacts(t *testing.T) {
	ctx := context.Background()
	dsn := startParadeDB(ctx, t)
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	q := db.New(st.Pool)

	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert organization: %v", err)
	}
	projA, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project a: %v", err)
	}
	projB, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "b"})
	if err != nil {
		t.Fatalf("insert project b: %v", err)
	}

	newRun := func(project pgtype.UUID) pgtype.UUID {
		t.Helper()
		run, err := q.InsertRun(ctx, project)
		if err != nil {
			t.Fatalf("insert run: %v", err)
		}
		return run.ID
	}
	upsert := func(project, run pgtype.UUID, entity, predicate, value string, seq int64) (int64, error) {
		return q.UpsertWorkingFact(ctx, db.UpsertWorkingFactParams{
			ProjectID: project, RunID: run, Entity: entity, Predicate: predicate,
			Value: []byte(value), Seq: seq, AgentID: "researcher",
		})
	}
	readFact := func(project, run pgtype.UUID, entity, predicate string) (value string, seq int64, agent string) {
		t.Helper()
		if err := st.Pool.QueryRow(ctx,
			`SELECT value::text, seq, agent_id FROM working_facts
			  WHERE project_id = $1 AND run_id = $2 AND entity = $3 AND predicate = $4`,
			project, run, entity, predicate).Scan(&value, &seq, &agent); err != nil {
			t.Fatalf("read fact: %v", err)
		}
		return value, seq, agent
	}

	// One subject, written twice: the second write replaces the first in place rather than adding a
	// row. Without ON CONFLICT the second insert is a unique violation; with DO NOTHING it silently
	// keeps the stale value, which is the original bug wearing a different hat.
	t.Run("upsert_collapses_a_subject_to_one_row", func(t *testing.T) {
		run := newRun(projA.ID)
		if rows, err := upsert(projA.ID, run, "auth", "status", `{"v":"up"}`, 1); err != nil || rows != 1 {
			t.Fatalf("first upsert = (%d, %v), want (1, nil)", rows, err)
		}
		if rows, err := upsert(projA.ID, run, "auth", "status", `{"v":"down"}`, 2); err != nil || rows != 1 {
			t.Fatalf("second upsert = (%d, %v), want (1, nil)", rows, err)
		}
		if got := count(ctx, t, st.Pool,
			`SELECT count(*) FROM working_facts WHERE run_id = $1`, run); got != 1 {
			t.Errorf("rows for the subject = %d, want 1 (the upsert must replace, not accumulate)", got)
		}
		value, seq, _ := readFact(projA.ID, run, "auth", "status")
		if seq != 2 || !strings.Contains(value, "down") {
			t.Errorf("stored fact = (%s, seq %d), want the seq-2 value", value, seq)
		}
	})

	// The guard: a write may only move a subject forward. An older seq is a no-op, and so is an
	// equal one — and crucially neither is an error, because the ingest handler must not roll back a
	// perfectly good event just because a newer value already won.
	t.Run("seq_guard_refuses_to_go_backwards", func(t *testing.T) {
		run := newRun(projA.ID)
		if _, err := upsert(projA.ID, run, "auth", "status", `{"v":"five"}`, 5); err != nil {
			t.Fatalf("seed at seq 5: %v", err)
		}

		rows, err := upsert(projA.ID, run, "auth", "status", `{"v":"three"}`, 3)
		if err != nil {
			t.Fatalf("older write returned an error, want a silent no-op: %v", err)
		}
		if rows != 0 {
			t.Errorf("older write affected %d rows, want 0", rows)
		}

		// Strictly less-than: a redelivery of the same seq must also be a no-op, which is what makes
		// the write idempotent under retry.
		if rows, err := upsert(projA.ID, run, "auth", "status", `{"v":"five-again"}`, 5); err != nil || rows != 0 {
			t.Errorf("equal-seq write = (%d, %v), want (0, nil)", rows, err)
		}

		if value, seq, _ := readFact(projA.ID, run, "auth", "status"); seq != 5 || !strings.Contains(value, `"five"`) {
			t.Errorf("after the suppressed writes the row is (%s, seq %d), want the original seq-5 value", value, seq)
		}

		if rows, err := upsert(projA.ID, run, "auth", "status", `{"v":"six"}`, 6); err != nil || rows != 1 {
			t.Errorf("newer write = (%d, %v), want (1, nil)", rows, err)
		}
		if _, seq, _ := readFact(projA.ID, run, "auth", "status"); seq != 6 {
			t.Errorf("seq after the newer write = %d, want 6", seq)
		}
	})

	// The key carries run_id. Two runs in ONE project holding the same subject are two independent
	// rows; if the key were project-scoped the second write would overwrite the first and a run
	// would lose its own working state to a sibling run.
	t.Run("key_is_run_scoped_not_project_scoped", func(t *testing.T) {
		run1, run2 := newRun(projA.ID), newRun(projA.ID)
		if _, err := upsert(projA.ID, run1, "task", "owner", `{"v":"alice"}`, 1); err != nil {
			t.Fatalf("run1 upsert: %v", err)
		}
		if _, err := upsert(projA.ID, run2, "task", "owner", `{"v":"bob"}`, 1); err != nil {
			t.Fatalf("run2 upsert: %v", err)
		}
		if v, _, _ := readFact(projA.ID, run1, "task", "owner"); !strings.Contains(v, "alice") {
			t.Errorf("run1 sees %s, want its own value — a sibling run overwrote it", v)
		}
		if v, _, _ := readFact(projA.ID, run2, "task", "owner"); !strings.Contains(v, "bob") {
			t.Errorf("run2 sees %s, want its own value", v)
		}
	})

	// The reads happen per run, so a run only ever sees its own facts.
	t.Run("list_returns_only_the_runs_own_facts_freshest_first", func(t *testing.T) {
		run, other := newRun(projA.ID), newRun(projA.ID)
		for _, f := range []struct {
			entity string
			seq    int64
		}{{"a", 1}, {"b", 5}, {"c", 3}} {
			if _, err := upsert(projA.ID, run, f.entity, "p", `{"v":1}`, f.seq); err != nil {
				t.Fatalf("seed %s: %v", f.entity, err)
			}
		}
		if _, err := upsert(projA.ID, other, "zzz", "p", `{"v":1}`, 9); err != nil {
			t.Fatalf("seed other run: %v", err)
		}

		got, err := q.ListRunWorkingFacts(ctx, db.ListRunWorkingFactsParams{ProjectID: projA.ID, RunID: run})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var seqs []int64
		for _, r := range got {
			if r.Entity == "zzz" {
				t.Error("another run's fact leaked into this run's list")
			}
			seqs = append(seqs, r.Seq)
		}
		// Freshest first — asserted as an exact sequence, because a presence check would survive a
		// reversed or missing ORDER BY.
		if len(seqs) != 3 || seqs[0] != 5 || seqs[1] != 3 || seqs[2] != 1 {
			t.Errorf("seq order = %v, want [5 3 1] (seq DESC)", seqs)
		}
	})

	// The subject-length constraints count BYTES, matching the ingest validator. A 128-rune string
	// of two-byte runes is 256 bytes and must be accepted; 129 of them is 258 bytes and must not.
	// Under a character-counting constraint the 129-rune case would wrongly pass.
	t.Run("constraints_reject_out_of_contract_rows", func(t *testing.T) {
		run := newRun(projA.ID)
		atLimit, overLimit := strings.Repeat("é", 128), strings.Repeat("é", 129)
		if len(atLimit) != 256 || len(overLimit) != 258 {
			t.Fatalf("fixture sizes = %d/%d bytes, want 256/258", len(atLimit), len(overLimit))
		}
		if _, err := upsert(projA.ID, run, atLimit, "p", `{"v":1}`, 1); err != nil {
			t.Errorf("a 256-byte entity should be accepted, got %v", err)
		}
		if _, err := upsert(projA.ID, run, overLimit, "p", `{"v":1}`, 1); pgErrCode(err) != "23514" {
			t.Errorf("a 258-byte entity should raise 23514 (check_violation), got %q", pgErrCode(err))
		}
		if _, err := upsert(projA.ID, run, "", "p", `{"v":1}`, 1); pgErrCode(err) != "23514" {
			t.Errorf("an empty entity should raise 23514, got %q", pgErrCode(err))
		}
		if _, err := upsert(projA.ID, run, "e", "", `{"v":1}`, 1); pgErrCode(err) != "23514" {
			t.Errorf("an empty predicate should raise 23514, got %q", pgErrCode(err))
		}
		// seq is 1-based; 0 covers nothing, so a zero here is a writer bug, not data.
		if _, err := upsert(projA.ID, run, "e", "p", `{"v":1}`, 0); pgErrCode(err) != "23514" {
			t.Errorf("seq 0 should raise 23514, got %q", pgErrCode(err))
		}
	})

	// The composite foreign key is what makes a fact's project agree with its run's project. A plain
	// run_id reference would accept this mismatched pair.
	t.Run("composite_fk_rejects_a_project_run_mismatch", func(t *testing.T) {
		runOfA := newRun(projA.ID)
		if _, err := upsert(projB.ID, runOfA, "e", "p", `{"v":1}`, 1); pgErrCode(err) != "23503" {
			t.Errorf("run A tagged with project B should raise 23503 (foreign_key_violation), got %q", pgErrCode(err))
		}
	})

	// Deleting a run — or the project above it — takes its working facts with it. This cascade is
	// what bounds the table's growth, so it is a correctness property, not housekeeping.
	t.Run("cascade_with_run_and_project", func(t *testing.T) {
		run := newRun(projA.ID)
		if _, err := upsert(projA.ID, run, "e", "p", `{"v":1}`, 1); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := st.Pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, run); err != nil {
			t.Fatalf("delete run: %v", err)
		}
		if got := count(ctx, t, st.Pool, `SELECT count(*) FROM working_facts WHERE run_id = $1`, run); got != 0 {
			t.Errorf("facts after deleting the run = %d, want 0", got)
		}

		projC, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "c"})
		if err != nil {
			t.Fatalf("insert project c: %v", err)
		}
		runC := newRun(projC.ID)
		if _, err := upsert(projC.ID, runC, "e", "p", `{"v":1}`, 1); err != nil {
			t.Fatalf("seed c: %v", err)
		}
		if _, err := st.Pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, projC.ID); err != nil {
			t.Fatalf("delete project: %v", err)
		}
		if got := count(ctx, t, st.Pool, `SELECT count(*) FROM working_facts WHERE project_id = $1`, projC.ID); got != 0 {
			t.Errorf("facts after deleting the project = %d, want 0", got)
		}
	})

	// The tenant policy actually filters under the app role, and an unset scope is fail-closed —
	// an omitted ENABLE ROW LEVEL SECURITY or a policy on the wrong column would leave the table
	// readable across tenants while every other test here still passed.
	t.Run("rls_scopes_reads_to_the_session_project", func(t *testing.T) {
		runA, runB := newRun(projA.ID), newRun(projB.ID)
		if _, err := upsert(projA.ID, runA, "shared", "p", `{"v":"a"}`, 1); err != nil {
			t.Fatalf("seed a: %v", err)
		}
		if _, err := upsert(projB.ID, runB, "shared", "p", `{"v":"b"}`, 1); err != nil {
			t.Fatalf("seed b: %v", err)
		}
		projBStr := uuid.UUID(projB.ID.Bytes).String()

		asAppRole(ctx, t, st.Pool, projBStr, func(tx pgx.Tx) {
			if got := count(ctx, t, tx, `SELECT count(*) FROM working_facts WHERE entity = 'shared'`); got != 1 {
				t.Errorf("shared-subject facts visible to B = %d, want 1 (only B's own)", got)
			}
			if got := count(ctx, t, tx, `SELECT count(*) FROM working_facts WHERE project_id = $1`, projA.ID); got != 0 {
				t.Errorf("A's facts visible to B = %d, want 0", got)
			}
		})
		asAppRole(ctx, t, st.Pool, "", func(tx pgx.Tx) {
			if got := count(ctx, t, tx, `SELECT count(*) FROM working_facts`); got != 0 {
				t.Errorf("facts with no scope = %d, want 0 (fail-closed)", got)
			}
		})
	})

	// Down drops the table with its policy and grants; Up restores it. Run last, because it removes
	// the table the other sub-tests use.
	t.Run("reversibility", func(t *testing.T) {
		sqlDB, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open sql.DB for goose: %v", err)
		}
		defer func() { _ = sqlDB.Close() }()
		provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
		if err != nil {
			t.Fatalf("create goose provider: %v", err)
		}
		if _, err := provider.DownTo(ctx, 18); err != nil {
			t.Fatalf("goose down to 0018 (revert 0019): %v", err)
		}
		if tableExists(ctx, t, st.Pool, "working_facts") {
			t.Error("down 0019 should drop working_facts")
		}
		if _, err := provider.UpTo(ctx, 19); err != nil {
			t.Fatalf("goose up to 0019 (reapply): %v", err)
		}
		if !tableExists(ctx, t, st.Pool, "working_facts") {
			t.Error("up 0019 should restore working_facts")
		}
		if !rlsEnabled(ctx, t, st.Pool, "working_facts") {
			t.Error("up 0019 should re-enable RLS on working_facts")
		}
	})
}
