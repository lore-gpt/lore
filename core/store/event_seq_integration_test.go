//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/store/migrations"
)

// TestEventSeqConcurrentInsert proves the write path's seq contract end to end: many writers
// racing InsertEvent on one run get exactly the values 1..N — no gap, no duplicate. The seq is
// assigned inside the query by a single-row UPDATE ... RETURNING whose row lock serialises the
// writers, so no advisory lock is needed, and the run counter ends exactly at N.
func TestEventSeqConcurrentInsert(t *testing.T) {
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
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	const writers = 50
	seqs := make([]int64, 0, writers)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte("{}")})
			if err != nil {
				errCh <- err
				return
			}
			mu.Lock()
			seqs = append(seqs, ev.Seq)
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent InsertEvent: %v", err)
	}

	if len(seqs) != writers {
		t.Fatalf("collected %d seqs, want %d", len(seqs), writers)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, s := range seqs {
		if s != int64(i+1) {
			t.Fatalf("sorted seq[%d] = %d, want %d — the write path produced a gap or duplicate", i, s, i+1)
		}
	}
	assertLastSeq(ctx, t, st.Pool, run.ID, writers)
}

// TestEventSeqReusedAfterRollback proves the gap-free-across-rollback guarantee the CTE advertises:
// because seq is bumped inside the write transaction, a rolled-back write gives its number back, so
// the next committed write REUSES it. The committed events stay contiguous with no hole.
func TestEventSeqReusedAfterRollback(t *testing.T) {
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
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	params := db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte("{}")}

	// A committed event takes seq 1.
	ev1, err := q.InsertEvent(ctx, params)
	if err != nil {
		t.Fatalf("first event: %v", err)
	}
	if ev1.Seq != 1 {
		t.Fatalf("first event seq = %d, want 1", ev1.Seq)
	}

	// A write that bumps the counter to 2 but then rolls back must give the number back.
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	evRB, err := db.New(tx).InsertEvent(ctx, params)
	if err != nil {
		t.Fatalf("event inside rolled-back tx: %v", err)
	}
	if evRB.Seq != 2 {
		t.Fatalf("event inside tx seq = %d, want 2", evRB.Seq)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// The next committed write REUSES 2 — gap-free across the rollback, not 3.
	ev2, err := q.InsertEvent(ctx, params)
	if err != nil {
		t.Fatalf("event after rollback: %v", err)
	}
	if ev2.Seq != 2 {
		t.Errorf("seq after rollback = %d, want 2 (the released number must be reused)", ev2.Seq)
	}
	// Only the two committed events exist and their seqs are contiguous [1, 2].
	assertSeqs(ctx, t, st.Pool, run.ID, []int64{1, 2})
	assertLastSeq(ctx, t, st.Pool, run.ID, 2)
}

// TestMigration0009EventsSeqNotNull proves migration 0009: it backfills any events written before
// the seq-aware write path (seq IS NULL), numbering them per run and continuing past that run's
// current maximum, advances each run counter, then makes events.seq NOT NULL — and it is
// reversible even with data present.
func TestMigration0009EventsSeqNotNull(t *testing.T) {
	ctx := context.Background()
	dsn := startParadeDB(ctx, t)

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB for goose: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("create goose provider: %v", err)
	}

	// Migrate up to 0008 only: events.seq is still nullable here, so we can create the pre-seq
	// rows that the backfill has to repair.
	if _, err := provider.UpTo(ctx, 8); err != nil {
		t.Fatalf("up to 0008: %v", err)
	}

	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	q := db.New(st.Pool)

	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	runA, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run A: %v", err)
	}
	runB, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run B: %v", err)
	}

	insertNullSeq := func(run pgtype.UUID) pgtype.UUID {
		var id pgtype.UUID
		if err := st.Pool.QueryRow(ctx,
			`INSERT INTO events (project_id, run_id, agent_id, payload) VALUES ($1, $2, 'a', '{}'::jsonb) RETURNING id`,
			proj.ID, run).Scan(&id); err != nil {
			t.Fatalf("insert null-seq event: %v", err)
		}
		return id
	}
	// Run A: three events written before seq assignment (all NULL), in a known write order. A NULL
	// seq is allowed at 0008.
	var runAIDs []pgtype.UUID
	for i := 0; i < 3; i++ {
		runAIDs = append(runAIDs, insertNullSeq(runA.ID))
	}
	// Run B: a pre-existing assigned seq=5, then two NULL-seq events after it — the backfill must
	// continue past the current max (not restart at 1) and preserve write order.
	var b5 pgtype.UUID
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO events (project_id, run_id, agent_id, payload, seq) VALUES ($1, $2, 'a', '{}'::jsonb, 5) RETURNING id`,
		proj.ID, runB.ID).Scan(&b5); err != nil {
		t.Fatalf("insert seq=5 event: %v", err)
	}
	b6 := insertNullSeq(runB.ID)
	b7 := insertNullSeq(runB.ID)

	// Apply 0009: backfill, then SET NOT NULL.
	if _, err := provider.UpTo(ctx, 9); err != nil {
		t.Fatalf("up to 0009 (backfill + not null): %v", err)
	}

	// Run A's three NULL events are numbered 1..3 in write order; the counter advances to 3.
	assertSeqs(ctx, t, st.Pool, runA.ID, []int64{1, 2, 3})
	assertSeqOrder(ctx, t, st.Pool, runA.ID, runAIDs)
	assertLastSeq(ctx, t, st.Pool, runA.ID, 3)
	// Run B's two NULL events continue past 5 as 6, 7 (earlier row -> 6, later -> 7); counter -> 7.
	assertSeqs(ctx, t, st.Pool, runB.ID, []int64{5, 6, 7})
	assertSeqOrder(ctx, t, st.Pool, runB.ID, []pgtype.UUID{b5, b6, b7})
	assertLastSeq(ctx, t, st.Pool, runB.ID, 7)

	// seq is now NOT NULL: a NULL insert is rejected.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO events (project_id, run_id, agent_id, payload) VALUES ($1, $2, 'a', '{}'::jsonb)`,
		proj.ID, runA.ID); pgErrCode(err) != "23502" {
		t.Errorf("a NULL seq insert after 0009 should raise 23502 (not_null_violation), got %q", pgErrCode(err))
	}

	// Reversibility with data present: Down 0009 loosens seq back to nullable (a NULL insert
	// succeeds again); Up 0009 backfills the fresh NULL row (numbered 4, after run A's max of 3)
	// and re-tightens to NOT NULL.
	if _, err := provider.DownTo(ctx, 8); err != nil {
		t.Fatalf("down to 0008 (revert 0009): %v", err)
	}
	insertNullSeq(runA.ID)
	if _, err := provider.UpTo(ctx, 9); err != nil {
		t.Fatalf("up to 0009 again (must backfill the new NULL row): %v", err)
	}
	assertLastSeq(ctx, t, st.Pool, runA.ID, 4)
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO events (project_id, run_id, agent_id, payload) VALUES ($1, $2, 'a', '{}'::jsonb)`,
		proj.ID, runA.ID); pgErrCode(err) != "23502" {
		t.Errorf("events.seq should be NOT NULL after re-applying 0009 (want 23502), got %q", pgErrCode(err))
	}
}

// assertSeqs checks that a run's events carry exactly the expected seq values in order.
func assertSeqs(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID pgtype.UUID, want []int64) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT seq FROM events WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		t.Fatalf("query seqs: %v", err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan seq: %v", err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate seqs: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("run seqs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("run seqs = %v, want %v", got, want)
		}
	}
}

// assertSeqOrder checks that ordering a run's events by seq yields exactly the given ids — i.e. the
// backfill numbered the rows in the expected write-time order, not just assigned the right set.
func assertSeqOrder(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID pgtype.UUID, wantIDs []pgtype.UUID) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id FROM events WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		t.Fatalf("query seq order: %v", err)
	}
	defer rows.Close()
	var got []pgtype.UUID
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ids: %v", err)
	}
	if len(got) != len(wantIDs) {
		t.Fatalf("run has %d events, want %d", len(got), len(wantIDs))
	}
	for i := range wantIDs {
		if got[i].Bytes != wantIDs[i].Bytes {
			t.Errorf("event at seq position %d = %x, want %x — backfill ordering is wrong", i, got[i].Bytes, wantIDs[i].Bytes)
		}
	}
}

// assertLastSeq checks a run's last_seq counter.
func assertLastSeq(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID pgtype.UUID, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(ctx, `SELECT last_seq FROM runs WHERE id = $1`, runID).Scan(&got); err != nil {
		t.Fatalf("read last_seq: %v", err)
	}
	if got != want {
		t.Errorf("last_seq = %d, want %d", got, want)
	}
}
