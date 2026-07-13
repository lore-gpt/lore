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

// TestMigration0011ClaimsEntityRegistry proves migration 0011: entities (project_id, name) becomes
// UNIQUE (so the write path can upsert by name), claims.memory_id becomes nullable (a standalone
// claim has none) while claims.source_event_id carries provenance on every claim, the memory
// composite FK still cascades to a linked claim, source_event_id is ON DELETE SET NULL, and the
// migration is reversible.
func TestMigration0011ClaimsEntityRegistry(t *testing.T) {
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

	if !columnExists(ctx, t, st.Pool, "claims", "source_event_id") {
		t.Fatal("claims should have gained source_event_id after 0011")
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
	if err := store.CreateProjectPartitions(ctx, st.Pool, proj.ID); err != nil {
		t.Fatalf("create partitions: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	ev, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}

	// Entity registry: UpsertEntity is get-or-create — the same (project, name) returns the same id,
	// and the unique constraint rejects a raw duplicate.
	authID, err := q.UpsertEntity(ctx, db.UpsertEntityParams{ProjectID: proj.ID, Name: "auth", Type: "service", Aliases: []string{"auth-svc"}})
	if err != nil {
		t.Fatalf("upsert entity: %v", err)
	}
	again, err := q.UpsertEntity(ctx, db.UpsertEntityParams{ProjectID: proj.ID, Name: "auth", Type: "other", Aliases: []string{}})
	if err != nil {
		t.Fatalf("re-upsert entity: %v", err)
	}
	if authID != again {
		t.Errorf("UpsertEntity should return the same id for the same (project,name), got %v vs %v", authID, again)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO entities (project_id, name, type) VALUES ($1, 'auth', 't')`, proj.ID); pgErrCode(err) != "23505" {
		t.Errorf("duplicate entity (project,name) should raise 23505 (unique_violation), got %q", pgErrCode(err))
	}

	// A memory (partitioned) for the memory-linked claim.
	mem, err := q.InsertMemory(ctx, db.InsertMemoryParams{ProjectID: proj.ID, Kind: "semantic", Content: "m"})
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	// A claim linked to that memory (predicate p1), carrying provenance.
	linkedID := randomUUID(ctx, t, st.Pool)
	if err := q.InsertClaim(ctx, db.InsertClaimParams{
		ID: linkedID, MemoryID: mem, ProjectID: proj.ID, EntityID: authID,
		Predicate: "p1", Value: []byte(`"x"`), SourceEventID: ev.ID,
	}); err != nil {
		t.Fatalf("insert memory-linked claim: %v", err)
	}

	// A standalone claim (memory_id NULL, predicate p2) — still traceable via source_event_id.
	standaloneID := randomUUID(ctx, t, st.Pool)
	if err := q.InsertClaim(ctx, db.InsertClaimParams{
		ID: standaloneID, ProjectID: proj.ID, EntityID: authID,
		Predicate: "p2", Value: []byte(`"y"`), SourceEventID: ev.ID,
	}); err != nil {
		t.Fatalf("insert standalone claim (memory_id NULL): %v", err)
	}
	var memoryIDSet bool
	if err := st.Pool.QueryRow(ctx, `SELECT memory_id IS NOT NULL FROM claims WHERE id = $1`, standaloneID).Scan(&memoryIDSet); err != nil {
		t.Fatalf("read standalone claim memory_id: %v", err)
	}
	if memoryIDSet {
		t.Error("standalone claim memory_id should be NULL")
	}

	// source_event_id is ON DELETE SET NULL: purging the source event clears the pointer but keeps the
	// claim. Use a dedicated event + claim so the others' provenance is untouched.
	ev2, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatalf("insert event 2: %v", err)
	}
	setNullID := randomUUID(ctx, t, st.Pool)
	if err := q.InsertClaim(ctx, db.InsertClaimParams{
		ID: setNullID, ProjectID: proj.ID, EntityID: authID,
		Predicate: "p3", Value: []byte(`"z"`), SourceEventID: ev2.ID,
	}); err != nil {
		t.Fatalf("insert claim referencing ev2: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM events WHERE id = $1`, ev2.ID); err != nil {
		t.Fatalf("delete ev2: %v", err)
	}
	var survives int
	var srcSet bool
	if err := st.Pool.QueryRow(ctx, `SELECT count(*), bool_or(source_event_id IS NOT NULL) FROM claims WHERE id = $1`, setNullID).
		Scan(&survives, &srcSet); err != nil {
		t.Fatalf("read claim after event delete: %v", err)
	}
	if survives != 1 {
		t.Error("deleting the source event must not delete the claim (ON DELETE SET NULL, not CASCADE)")
	}
	if srcSet {
		t.Error("deleting the source event should NULL the claim's source_event_id")
	}

	// The memory composite FK still cascades: deleting the memory removes its linked claim while the
	// standalone claim survives.
	if _, err := st.Pool.Exec(ctx, `DELETE FROM memories WHERE id = $1`, mem); err != nil {
		t.Fatalf("delete memory: %v", err)
	}
	var linkedCount, standaloneCount int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM claims WHERE id = $1`, linkedID).Scan(&linkedCount); err != nil {
		t.Fatalf("count linked claim: %v", err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM claims WHERE id = $1`, standaloneID).Scan(&standaloneCount); err != nil {
		t.Fatalf("count standalone claim: %v", err)
	}
	if linkedCount != 0 {
		t.Error("deleting the memory should cascade-delete its linked claim")
	}
	if standaloneCount != 1 {
		t.Error("the standalone (memory_id NULL) claim should survive deleting an unrelated memory")
	}

	// Reversibility: clear the remaining standalone (NULL-memory) claims so SET NOT NULL holds, then
	// Down 0011 drops the new column, Up 0011 restores it.
	if _, err := st.Pool.Exec(ctx, `DELETE FROM claims`); err != nil {
		t.Fatalf("clear claims before down: %v", err)
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB for goose: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("create goose provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 10); err != nil {
		t.Fatalf("down to 0010 (revert 0011): %v", err)
	}
	if columnExists(ctx, t, st.Pool, "claims", "source_event_id") {
		t.Error("down 0011 should drop claims.source_event_id")
	}
	if _, err := provider.UpTo(ctx, 11); err != nil {
		t.Fatalf("up to 0011 (reapply): %v", err)
	}
	if !columnExists(ctx, t, st.Pool, "claims", "source_event_id") {
		t.Error("up 0011 should restore claims.source_event_id")
	}
	// The unique entity constraint is back after re-applying: a raw duplicate is rejected again.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO entities (project_id, name, type) VALUES ($1, 'auth', 't')`, proj.ID); pgErrCode(err) != "23505" {
		t.Errorf("after reapply, duplicate entity should raise 23505, got %q", pgErrCode(err))
	}
}
