//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/store/migrations"
)

// TestMigration0012ExtractionModeAndBatchState proves migration 0012: projects gains an
// extraction_mode ('realtime' default, CHECK-constrained), runs gains nullable batch-state columns,
// GetRunExtractionState reads mode + pending batch in one query, SetRunBatch records a submitted
// batch, AdvanceCoveredSeq clears it while advancing the checkpoint, and all three columns are
// cleanly reversible.
func TestMigration0012ExtractionModeAndBatchState(t *testing.T) {
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

	for _, c := range []struct{ table, col string }{
		{"projects", "extraction_mode"},
		{"runs", "extraction_batch_id"},
		{"runs", "extraction_batch_covered_seq"},
	} {
		if !columnExists(ctx, t, st.Pool, c.table, c.col) {
			t.Fatalf("%s should have gained %s after 0012", c.table, c.col)
		}
	}

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

	state := func() db.GetRunExtractionStateRow {
		t.Helper()
		s, err := q.GetRunExtractionState(ctx, db.GetRunExtractionStateParams{RunID: run.ID, ProjectID: proj.ID})
		if err != nil {
			t.Fatalf("GetRunExtractionState: %v", err)
		}
		return s
	}

	// A fresh run: realtime mode (the project default) and no batch in flight.
	if s := state(); s.ExtractionMode != "realtime" {
		t.Errorf("fresh extraction_mode = %q, want realtime", s.ExtractionMode)
	}
	if s := state(); s.ExtractionBatchID != nil || s.ExtractionBatchCoveredSeq != nil {
		t.Errorf("fresh run has a pending batch: id=%v seq=%v", s.ExtractionBatchID, s.ExtractionBatchCoveredSeq)
	}

	// The CHECK rejects an unknown mode; a valid one (economy) reads back.
	if _, err := st.Pool.Exec(ctx, `UPDATE projects SET extraction_mode = 'bogus' WHERE id = $1`, proj.ID); err == nil {
		t.Error("extraction_mode CHECK should reject 'bogus'")
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE projects SET extraction_mode = 'economy' WHERE id = $1`, proj.ID); err != nil {
		t.Fatalf("set economy: %v", err)
	}
	if s := state(); s.ExtractionMode != "economy" {
		t.Errorf("extraction_mode = %q, want economy", s.ExtractionMode)
	}

	// SetRunBatch records the submitted batch; GetRunExtractionState reads it back.
	handle, covered := "batch_abc", int64(7)
	if rows, err := q.SetRunBatch(ctx, db.SetRunBatchParams{
		BatchID: &handle, BatchCoveredSeq: &covered, RunID: run.ID, ProjectID: proj.ID,
	}); err != nil || rows != 1 {
		t.Fatalf("SetRunBatch: rows=%d err=%v, want 1 row", rows, err)
	}
	if s := state(); s.ExtractionBatchID == nil || *s.ExtractionBatchID != handle ||
		s.ExtractionBatchCoveredSeq == nil || *s.ExtractionBatchCoveredSeq != covered {
		t.Errorf("pending batch = {id:%v seq:%v}, want {%q %d}", s.ExtractionBatchID, s.ExtractionBatchCoveredSeq, handle, covered)
	}

	// AdvanceCoveredSeq advances the checkpoint (from its still-0 start, since SetRunBatch moved only the
	// batch columns) AND clears the batch state in one update.
	if rows, err := q.AdvanceCoveredSeq(ctx, db.AdvanceCoveredSeqParams{
		ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: 0, NewCoveredSeq: covered,
	}); err != nil || rows != 1 {
		t.Fatalf("AdvanceCoveredSeq to %d: rows=%d err=%v, want 1 row", covered, rows, err)
	}
	if s := state(); s.ExtractionBatchID != nil || s.ExtractionBatchCoveredSeq != nil {
		t.Errorf("AdvanceCoveredSeq must clear the batch state, got id=%v seq=%v", s.ExtractionBatchID, s.ExtractionBatchCoveredSeq)
	}
	var covSeq int64
	if err := st.Pool.QueryRow(ctx, `SELECT covered_seq FROM runs WHERE id = $1`, run.ID).Scan(&covSeq); err != nil {
		t.Fatalf("read covered_seq: %v", err)
	}
	if covSeq != covered {
		t.Errorf("covered_seq = %d, want %d (advanced alongside the batch clear)", covSeq, covered)
	}

	// Reversibility: Down 0012 drops all three columns, Up 0012 restores them.
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB for goose: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("create goose provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 11); err != nil {
		t.Fatalf("down to 0011 (revert 0012): %v", err)
	}
	for _, c := range []struct{ table, col string }{
		{"projects", "extraction_mode"},
		{"runs", "extraction_batch_id"},
		{"runs", "extraction_batch_covered_seq"},
	} {
		if columnExists(ctx, t, st.Pool, c.table, c.col) {
			t.Errorf("down 0012 should drop %s.%s", c.table, c.col)
		}
	}
	if _, err := provider.UpTo(ctx, 12); err != nil {
		t.Fatalf("up to 0012 (reapply): %v", err)
	}
	if !columnExists(ctx, t, st.Pool, "projects", "extraction_mode") {
		t.Error("up 0012 should restore projects.extraction_mode")
	}
}
