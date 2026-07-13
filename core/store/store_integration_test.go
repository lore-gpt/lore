//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/store/migrations"
)

// Pinned image: ships pgvector + pg_search. Keep in lockstep with
// infra/docker-compose.yml.
const paradeDBImage = "paradedb/paradedb:0.24.2-pg17"

// TestMigrationsExtensionsAndRoundTrip proves, against a real ParadeDB, that:
// migrations apply (and are idempotent), the vector + pg_search extensions load,
// the core tables exist, and the org->project->run->event chain round-trips
// through the sqlc queries.
func TestMigrationsExtensionsAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	dsn := startParadeDB(ctx, t)

	// Idempotent: running twice must succeed (every boot calls this).
	for pass := 1; pass <= 2; pass++ {
		if err := store.RunMigrations(ctx, dsn); err != nil {
			t.Fatalf("run migrations (pass %d): %v", pass, err)
		}
	}

	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	for _, ext := range []string{"vector", "pg_search"} {
		var exists bool
		if err := st.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)`, ext).Scan(&exists); err != nil {
			t.Fatalf("query extension %s: %v", ext, err)
		}
		if !exists {
			t.Errorf("extension %q not installed", ext)
		}
	}

	for _, tbl := range []string{"organizations", "projects", "api_keys", "runs", "events", "memories"} {
		if !tableExists(ctx, t, st.Pool, tbl) {
			t.Errorf("table %q missing", tbl)
		}
	}

	// Migration 0002 completes the memories table: every column it adds must be
	// present and the Phase 0 inline `embedding` column must be gone (embeddings
	// relocate to their own dimension-free table in a later migration).
	for _, col := range []string{
		"entities", "valid_from", "valid_to", "superseded_by",
		"trust_tier", "review_status", "created_by_agent", "source_event_id",
	} {
		if !memoriesHasColumn(ctx, t, st.Pool, col) {
			t.Errorf("memories column %q missing after 0002", col)
		}
	}
	if memoriesHasColumn(ctx, t, st.Pool, "embedding") {
		t.Error("memories.embedding should be dropped by 0002")
	}

	q := db.New(st.Pool)

	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert organization: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "platform"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	// memories/embeddings are LIST-partitioned by project_id (0006) with no default
	// partition, so a project's partition must exist before any memory is written.
	if err := store.CreateProjectPartitions(ctx, st.Pool, proj.ID); err != nil {
		t.Fatalf("create project partitions: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	payload, err := json.Marshal(map[string]string{"msg": "hello memory"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	ev, err := q.InsertEvent(ctx, db.InsertEventParams{
		RunID:   run.ID,
		AgentID: "researcher",
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if ev.Seq != 1 {
		t.Errorf("first event in a fresh run should get seq 1, got %d", ev.Seq)
	}

	got, err := q.GetEvent(ctx, db.GetEventParams{ProjectID: ev.ProjectID, ID: ev.ID})
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if got.AgentID != "researcher" {
		t.Errorf("agent_id = %q, want %q", got.AgentID, "researcher")
	}

	count, err := q.CountAllEvents(ctx)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 1 {
		t.Errorf("event count = %d, want 1", count)
	}

	// A memory written with only the required columns picks up the migration 0002
	// defaults. Governance columns default to basic OSS behavior (trust_tier=normal,
	// review_status=auto_approved); entities defaults to an empty JSON array.
	var (
		trustTier, reviewStatus string
		version                 int32
		entities                []byte
	)
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO memories (project_id, kind, content)
		 VALUES ($1, 'semantic', 'the sky is blue')
		 RETURNING trust_tier, review_status, version, entities`,
		proj.ID).Scan(&trustTier, &reviewStatus, &version, &entities); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	if trustTier != "normal" {
		t.Errorf("trust_tier default = %q, want %q", trustTier, "normal")
	}
	if reviewStatus != "auto_approved" {
		t.Errorf("review_status default = %q, want %q", reviewStatus, "auto_approved")
	}
	if version != 1 {
		t.Errorf("version default = %d, want 1", version)
	}
	if string(entities) != "[]" {
		t.Errorf("entities default = %q, want %q", string(entities), "[]")
	}

	// The kind CHECK constraint rejects values outside the closed vocabulary.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO memories (project_id, kind, content) VALUES ($1, 'bogus', 'x')`,
		proj.ID); err == nil {
		t.Error("memories_kind_check should reject kind='bogus'")
	}

	// Migration 0002 is cleanly reversible. RunMigrations only exposes Up, so drive a
	// goose provider directly. With 0003 stacked on top, migrate down to version 1
	// (reverting 0003 then 0002) so 0002's Down runs — the inline embedding column
	// returns and the added columns disappear — then reapply up through 0002.
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB for goose: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("create goose provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 1); err != nil {
		t.Fatalf("goose down to 0001 (revert 0002): %v", err)
	}
	if !memoriesHasColumn(ctx, t, st.Pool, "embedding") {
		t.Error("down 0002 should restore memories.embedding")
	}
	if memoriesHasColumn(ctx, t, st.Pool, "entities") {
		t.Error("down 0002 should drop memories.entities")
	}
	if _, err := provider.UpTo(ctx, 2); err != nil {
		t.Fatalf("goose up to 0002 (reapply 0002): %v", err)
	}
	if memoriesHasColumn(ctx, t, st.Pool, "embedding") {
		t.Error("up 0002 should drop memories.embedding again")
	}
	if !memoriesHasColumn(ctx, t, st.Pool, "entities") {
		t.Error("up 0002 should restore memories.entities")
	}
}

// memoriesHasColumn reports whether the memories table currently has the named
// column, per information_schema. Used to assert migration 0002's up/down effects.
func memoriesHasColumn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, column string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'memories' AND column_name = $1)`,
		column).Scan(&exists); err != nil {
		t.Fatalf("query memories column %q: %v", column, err)
	}
	return exists
}

// TestMigration0003VersionsClaimsScopes proves, against a real ParadeDB, that
// migration 0003 lands the version-history, claims, and scope tables; that the
// claims partial-unique index admits exactly one active claim per subject (and lets
// a subject be re-stated via a deferred-FK supersede-then-insert swap); that
// versions and scopes round-trip through the sqlc queries with their constraints
// enforced; and that the migration is cleanly reversible.
func TestMigration0003VersionsClaimsScopes(t *testing.T) {
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

	// The three new tables exist and memories gained the denormalized scope_keys column.
	for _, tbl := range []string{"memory_versions", "claims", "memory_scopes"} {
		if !tableExists(ctx, t, st.Pool, tbl) {
			t.Errorf("table %q missing after 0003", tbl)
		}
	}
	if !memoriesHasColumn(ctx, t, st.Pool, "scope_keys") {
		t.Error("migration 0003 should add memories.scope_keys")
	}

	q := db.New(st.Pool)

	// Seed the org -> project -> memory chain the new rows reference.
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert organization: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "platform"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := store.CreateProjectPartitions(ctx, st.Pool, proj.ID); err != nil {
		t.Fatalf("create project partitions: %v", err)
	}
	mem := insertMemory(ctx, t, st.Pool, proj.ID)

	// --- memory_versions: full history round-trips in version order. ---
	if _, err := q.InsertMemoryVersion(ctx, db.InsertMemoryVersionParams{
		ProjectID: proj.ID, MemoryID: mem, Version: 1, Content: "first",
	}); err != nil {
		t.Fatalf("insert memory version 1: %v", err)
	}
	changedBy, reason := "agent-x", "lww"
	if _, err := q.InsertMemoryVersion(ctx, db.InsertMemoryVersionParams{
		ProjectID: proj.ID, MemoryID: mem, Version: 2, Content: "second", ChangedBy: &changedBy, Reason: &reason,
	}); err != nil {
		t.Fatalf("insert memory version 2: %v", err)
	}
	versions, err := q.ListMemoryVersions(ctx, db.ListMemoryVersionsParams{ProjectID: proj.ID, MemoryID: mem})
	if err != nil {
		t.Fatalf("list memory versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("memory versions = %d, want 2", len(versions))
	}
	if versions[0].Version != 1 || versions[1].Version != 2 {
		t.Errorf("versions out of order: got %d, %d", versions[0].Version, versions[1].Version)
	}
	if versions[0].ChangedBy != nil {
		t.Errorf("version 1 changed_by = %q, want nil", *versions[0].ChangedBy)
	}
	if versions[1].Reason == nil || *versions[1].Reason != "lww" {
		t.Errorf("version 2 reason = %v, want %q", versions[1].Reason, "lww")
	}
	// The (project_id, memory_id, version) primary key rejects a duplicate version.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO memory_versions (project_id, memory_id, version, content) VALUES ($1, $2, 2, 'dup')`,
		proj.ID, mem); pgErrCode(err) != "23505" {
		t.Errorf("duplicate (project_id, memory_id, version) should raise 23505 (unique_violation), got %q", pgErrCode(err))
	}

	// --- claims: the partial-unique index allows one active claim per subject. ---
	// A subject is (project_id, entity_id, predicate). entity_id is a free-standing
	// uuid until the entity registry migration adds a reference.
	entityID := randomUUID(ctx, t, st.Pool)
	firstID := randomUUID(ctx, t, st.Pool)
	if err := q.InsertClaim(ctx, db.InsertClaimParams{
		ID: firstID, MemoryID: mem, ProjectID: proj.ID, EntityID: entityID,
		Predicate: "status", Value: []byte(`"open"`),
	}); err != nil {
		t.Fatalf("insert first claim: %v", err)
	}

	// A second ACTIVE claim about the same subject is rejected by claims_active_subject_key.
	_, err = st.Pool.Exec(ctx,
		`INSERT INTO claims (memory_id, project_id, entity_id, predicate, value)
		 VALUES ($1, $2, $3, 'status', '"closed"')`,
		mem, proj.ID, entityID)
	if pgErrCode(err) != "23505" {
		t.Errorf("second active claim should raise 23505 (unique_violation), got %q", pgErrCode(err))
	}

	// Re-state the subject in one transaction: supersede the current claim, then insert
	// its replacement with the id it now points at. The deferred self-FK lets the pointer
	// be set before the replacement row exists (validated at commit), and once the old
	// row is superseded the partial-unique index admits the new active row.
	replacementID := randomUUID(ctx, t, st.Pool)
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin swap tx: %v", err)
	}
	rows, err := q.WithTx(tx).SupersedeClaim(ctx, db.SupersedeClaimParams{
		ID: firstID, SupersededBy: replacementID, ProjectID: proj.ID,
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("supersede first claim: %v", err)
	}
	if rows != 1 {
		_ = tx.Rollback(ctx)
		t.Fatalf("supersede affected %d rows, want 1 (the active claim)", rows)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO claims (id, memory_id, project_id, entity_id, predicate, value)
		 VALUES ($1, $2, $3, $4, 'status', '"closed"')`,
		replacementID, mem, proj.ID, entityID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert replacement claim: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit claim swap (deferred self-FK should hold at commit): %v", err)
	}

	// After the swap exactly one active claim remains for the subject, and the first
	// claim now points at its replacement.
	var activeCount int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM claims
		 WHERE project_id = $1 AND entity_id = $2 AND predicate = 'status'
		   AND superseded_by IS NULL`,
		proj.ID, entityID).Scan(&activeCount); err != nil {
		t.Fatalf("count active claims: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("active claims for subject = %d, want 1", activeCount)
	}
	var supersededBy pgtype.UUID
	if err := st.Pool.QueryRow(ctx,
		`SELECT superseded_by FROM claims WHERE id = $1`, firstID).Scan(&supersededBy); err != nil {
		t.Fatalf("read superseded_by: %v", err)
	}
	if supersededBy != replacementID {
		t.Error("first claim should be superseded by the replacement")
	}

	// The self-FK actually exists and is validated at COMMIT (deferred), not skipped:
	// superseding a claim to a replacement id that is never inserted must fail the
	// commit — a missing FK would instead let this commit succeed.
	danglingTx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin dangling tx: %v", err)
	}
	danglingID := randomUUID(ctx, t, st.Pool)
	if _, err := danglingTx.Exec(ctx,
		`UPDATE claims SET superseded_by = $1 WHERE id = $2`, danglingID, replacementID); err != nil {
		_ = danglingTx.Rollback(ctx)
		t.Fatalf("deferred FK should not fire on the update statement, got: %v", err)
	}
	if err := danglingTx.Commit(ctx); pgErrCode(err) != "23503" {
		t.Errorf("dangling superseded_by should fail commit with 23503 (foreign_key_violation), got %q", pgErrCode(err))
	}

	// --- memory_scopes: tags round-trip and the scope_type vocabulary is enforced. ---
	if _, err := q.InsertMemoryScope(ctx, db.InsertMemoryScopeParams{
		ProjectID: proj.ID, MemoryID: mem, ScopeType: "run", ScopeID: "run-123",
	}); err != nil {
		t.Fatalf("insert run scope: %v", err)
	}
	if _, err := q.InsertMemoryScope(ctx, db.InsertMemoryScopeParams{
		ProjectID: proj.ID, MemoryID: mem, ScopeType: "agent", ScopeID: "researcher",
	}); err != nil {
		t.Fatalf("insert agent scope: %v", err)
	}
	scopes, err := q.ListMemoryScopes(ctx, db.ListMemoryScopesParams{ProjectID: proj.ID, MemoryID: mem})
	if err != nil {
		t.Fatalf("list memory scopes: %v", err)
	}
	if len(scopes) != 2 {
		t.Errorf("memory scopes = %d, want 2", len(scopes))
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO memory_scopes (project_id, memory_id, scope_type, scope_id) VALUES ($1, $2, 'bogus', 'x')`,
		proj.ID, mem); pgErrCode(err) != "23514" {
		t.Errorf("scope_type CHECK should raise 23514 (check_violation), got %q", pgErrCode(err))
	}

	// Migration 0003 is cleanly reversible: Down drops the three tables and the
	// scope_keys column; Up restores them. Target 0003 by version (down to 0002, back
	// up through 0003) so the check stays correct as later migrations stack on top.
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB for goose: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("create goose provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 2); err != nil {
		t.Fatalf("goose down to 0002 (revert 0003): %v", err)
	}
	for _, tbl := range []string{"memory_versions", "claims", "memory_scopes"} {
		if tableExists(ctx, t, st.Pool, tbl) {
			t.Errorf("down 0003 should drop the %q table", tbl)
		}
	}
	if memoriesHasColumn(ctx, t, st.Pool, "scope_keys") {
		t.Error("down 0003 should drop memories.scope_keys")
	}
	if _, err := provider.UpTo(ctx, 3); err != nil {
		t.Fatalf("goose up to 0003 (reapply 0003): %v", err)
	}
	for _, tbl := range []string{"memory_versions", "claims", "memory_scopes"} {
		if !tableExists(ctx, t, st.Pool, tbl) {
			t.Errorf("up 0003 should restore the %q table", tbl)
		}
	}
	if !memoriesHasColumn(ctx, t, st.Pool, "scope_keys") {
		t.Error("up 0003 should restore memories.scope_keys")
	}
}

// startParadeDB boots a throwaway ParadeDB container and returns its DSN. The
// container is terminated when the test finishes.
func startParadeDB(ctx context.Context, t *testing.T) string {
	t.Helper()
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
	return dsn
}

// tableExists reports whether a public table of the given name exists.
func tableExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name = $1)`,
		table).Scan(&exists); err != nil {
		t.Fatalf("query table %q: %v", table, err)
	}
	return exists
}

// insertMemory inserts a minimal memory row and returns its id, for use as a parent
// of the version / claim / scope rows.
func insertMemory(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID pgtype.UUID) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO memories (project_id, kind, content)
		 VALUES ($1, 'semantic', 'seed') RETURNING id`,
		projectID).Scan(&id); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	return id
}

// randomUUID returns a fresh uuid from the database, used for the free-standing
// entity_id and the replacement claim id.
func randomUUID(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT gen_random_uuid()`).Scan(&id); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	return id
}

// pgErrCode returns the Postgres SQLSTATE of err, or "" if err is nil or not a
// Postgres error. It lets the constraint-rejection assertions pin the exact
// SQLSTATE (e.g. 23505 unique_violation) rather than accepting any error.
func pgErrCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
