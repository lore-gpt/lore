//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pressly/goose/v3"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/store/migrations"
)

// TestMigration0007SeqAuditRetention proves, against a real ParadeDB, the sequence and
// append-only schema that migration 0007 adds: the per-run counter is monotonic and
// gap-free under concurrency, audit_log is append-only at the
// database layer (UPDATE/DELETE/TRUNCATE all rejected) and outlives the project it records,
// pack_logs keeps its trace when its run is deleted, and the whole migration is reversible.
func TestMigration0007SeqAuditRetention(t *testing.T) {
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
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	t.Run("schema", func(t *testing.T) {
		for tbl, col := range map[string]string{
			"runs":     "last_seq",
			"events":   "seq",
			"projects": "retain_events_days",
		} {
			if !columnExists(ctx, t, st.Pool, tbl, col) {
				t.Errorf("%s.%s should exist after 0007", tbl, col)
			}
		}
		if !columnExists(ctx, t, st.Pool, "projects", "retain_memories_days") {
			t.Error("projects.retain_memories_days should exist after 0007")
		}
		for _, tbl := range []string{"pack_logs", "audit_log"} {
			if !tableExists(ctx, t, st.Pool, tbl) {
				t.Errorf("%s table should exist after 0007", tbl)
			}
		}
	})

	// Many writers race UPDATE runs SET last_seq = last_seq + 1 RETURNING on one run. The
	// single-row update serialises on the row lock, so the returned values must be exactly
	// 1..N with no gap and no duplicate — a monotonic, gap-free counter without an advisory
	// lock (proven earlier on a throwaway schema; here it rides the real column).
	t.Run("seq_monotonic_under_concurrency", func(t *testing.T) {
		run, err := q.InsertRun(ctx, proj.ID)
		if err != nil {
			t.Fatalf("insert run: %v", err)
		}
		const writers, perWriter = 10, 100
		total := writers * perWriter
		seqs := make([]int64, 0, total)
		var mu sync.Mutex
		var wg sync.WaitGroup
		errCh := make(chan error, writers)
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perWriter; i++ {
					var s int64
					if err := st.Pool.QueryRow(ctx,
						`UPDATE runs SET last_seq = last_seq + 1 WHERE id = $1 RETURNING last_seq`,
						run.ID).Scan(&s); err != nil {
						errCh <- err
						return
					}
					mu.Lock()
					seqs = append(seqs, s)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatalf("concurrent last_seq update: %v", err)
		}
		if len(seqs) != total {
			t.Fatalf("collected %d seqs, want %d", len(seqs), total)
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		for i, s := range seqs {
			if s != int64(i+1) {
				t.Fatalf("sorted seq[%d] = %d, want %d — sequence has a gap or a duplicate", i, s, i+1)
			}
		}
	})

	// The UNIQUE(run_id, seq) constraint the migration adds: seq is unique within a run but not
	// across runs. The seq-aware write path assigns 1, 2, ... monotonically per run; re-using an
	// assigned seq is rejected, and the same seq in another run is fine.
	t.Run("events_seq_uniqueness", func(t *testing.T) {
		run, err := q.InsertRun(ctx, proj.ID)
		if err != nil {
			t.Fatalf("insert run: %v", err)
		}
		// InsertEvent stamps a monotonic per-run seq: the first is 1, the second 2.
		ev1, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte("{}")})
		if err != nil {
			t.Fatalf("first event should insert: %v", err)
		}
		if ev1.Seq != 1 {
			t.Errorf("first event seq = %d, want 1", ev1.Seq)
		}
		ev2, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte("{}")})
		if err != nil {
			t.Fatalf("second event should insert: %v", err)
		}
		if ev2.Seq != 2 {
			t.Errorf("second event seq = %d, want 2", ev2.Seq)
		}
		// Re-using an assigned seq within the run violates UNIQUE(run_id, seq).
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO events (project_id, run_id, agent_id, payload, seq) VALUES ($1, $2, 'a', '{}'::jsonb, 1)`,
			proj.ID, run.ID); pgErrCode(err) != "23505" {
			t.Errorf("re-using seq=1 in the same run should raise 23505, got %q", pgErrCode(err))
		}
		// The same seq in a DIFFERENT run is fine — uniqueness is scoped to the run.
		run2, err := q.InsertRun(ctx, proj.ID)
		if err != nil {
			t.Fatalf("insert second run: %v", err)
		}
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO events (project_id, run_id, agent_id, payload, seq) VALUES ($1, $2, 'a', '{}'::jsonb, 1)`,
			proj.ID, run2.ID); err != nil {
			t.Errorf("seq=1 in a different run should insert (uniqueness is per run): %v", err)
		}
	})

	// audit_log is append-only in the database, independent of any role grant: a row can be
	// inserted, but UPDATE, DELETE and TRUNCATE are all rejected by triggers, and the row
	// stays put after every attempt.
	t.Run("audit_log_append_only", func(t *testing.T) {
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO audit_log (actor, action) VALUES ('system', 'seed')`); err != nil {
			t.Fatalf("insert audit row: %v", err)
		}
		if _, err := st.Pool.Exec(ctx, `UPDATE audit_log SET action = 'tamper'`); pgErrCode(err) != "23001" {
			t.Errorf("UPDATE on audit_log should raise 23001 (append-only), got %q", pgErrCode(err))
		}
		if _, err := st.Pool.Exec(ctx, `DELETE FROM audit_log`); pgErrCode(err) != "23001" {
			t.Errorf("DELETE on audit_log should raise 23001 (append-only), got %q", pgErrCode(err))
		}
		if _, err := st.Pool.Exec(ctx, `TRUNCATE audit_log`); pgErrCode(err) != "23001" {
			t.Errorf("TRUNCATE on audit_log should raise 23001 (append-only), got %q", pgErrCode(err))
		}
		var n int
		if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		if n != 1 {
			t.Errorf("audit_log rows after rejected mutations = %d, want 1 (nothing removed)", n)
		}
	})

	// An erasure record has to outlive the project whose deletion it records, so audit_log's
	// project_id is a bare uuid with no foreign key: dropping the project leaves the row.
	t.Run("audit_log_outlives_project", func(t *testing.T) {
		projDel, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "doomed"})
		if err != nil {
			t.Fatalf("insert doomed project: %v", err)
		}
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO audit_log (project_id, actor, action, target) VALUES ($1, 'system', 'erasure', 'project')`,
			projDel.ID); err != nil {
			t.Fatalf("insert erasure record: %v", err)
		}
		if _, err := st.Pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, projDel.ID); err != nil {
			t.Fatalf("delete project: %v", err)
		}
		var n int
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE project_id = $1`, projDel.ID).Scan(&n); err != nil {
			t.Fatalf("count erasure record: %v", err)
		}
		if n != 1 {
			t.Error("the erasure record must survive deletion of the project it records")
		}
	})

	// pack_logs has an asymmetric FK pair: run_id is ON DELETE SET NULL (deleting a run keeps
	// the trace, just nulls the pointer), while project_id is ON DELETE CASCADE (deleting the
	// project takes its traces with it). Both sides are checked.
	t.Run("pack_logs_fk_behavior", func(t *testing.T) {
		run, err := q.InsertRun(ctx, proj.ID)
		if err != nil {
			t.Fatalf("insert run: %v", err)
		}
		var packID pgtype.UUID
		if err := st.Pool.QueryRow(ctx,
			`INSERT INTO pack_logs (project_id, run_id, query) VALUES ($1, $2, 'q') RETURNING id`,
			proj.ID, run.ID).Scan(&packID); err != nil {
			t.Fatalf("insert pack log: %v", err)
		}
		if _, err := st.Pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, run.ID); err != nil {
			t.Fatalf("delete run: %v", err)
		}
		var runNull bool
		if err := st.Pool.QueryRow(ctx,
			`SELECT run_id IS NULL FROM pack_logs WHERE id = $1`, packID).Scan(&runNull); err != nil {
			t.Fatalf("read pack log after run delete (it should survive): %v", err)
		}
		if !runNull {
			t.Error("deleting the run should null pack_logs.run_id, not delete the trace")
		}

		// project_id CASCADE: a disposable project's pack_logs vanish when the project does.
		projDel, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "packdoomed"})
		if err != nil {
			t.Fatalf("insert disposable project: %v", err)
		}
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO pack_logs (project_id, query) VALUES ($1, 'q')`, projDel.ID); err != nil {
			t.Fatalf("insert pack log for disposable project: %v", err)
		}
		if _, err := st.Pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, projDel.ID); err != nil {
			t.Fatalf("delete disposable project: %v", err)
		}
		var remaining int
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM pack_logs WHERE project_id = $1`, projDel.ID).Scan(&remaining); err != nil {
			t.Fatalf("count pack logs after project delete: %v", err)
		}
		if remaining != 0 {
			t.Errorf("deleting the project should cascade-delete its pack_logs rows, %d left", remaining)
		}
	})

	// 0007 is cleanly reversible: Down removes the columns, tables, and audit guards; Up
	// restores them. Runs last, since it rewinds the schema.
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
		if _, err := provider.DownTo(ctx, 6); err != nil {
			t.Fatalf("goose down to 0006 (revert 0007): %v", err)
		}
		if columnExists(ctx, t, st.Pool, "runs", "last_seq") {
			t.Error("down 0007 should drop runs.last_seq")
		}
		if tableExists(ctx, t, st.Pool, "audit_log") {
			t.Error("down 0007 should drop audit_log")
		}
		if _, err := provider.UpTo(ctx, 7); err != nil {
			t.Fatalf("goose up to 0007 (reapply): %v", err)
		}
		if !columnExists(ctx, t, st.Pool, "runs", "last_seq") {
			t.Error("up 0007 should restore runs.last_seq")
		}
		if !tableExists(ctx, t, st.Pool, "audit_log") {
			t.Error("up 0007 should restore audit_log")
		}
	})
}
