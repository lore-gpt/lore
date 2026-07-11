//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/store/migrations"
)

// Pinned and validated in PR-04: ships pgvector + pg_search. Keep in lockstep
// with infra/docker-compose.yml.
const paradeDBImage = "paradedb/paradedb:0.24.2-pg17"

// TestMigrationsExtensionsAndRoundTrip proves, against a real ParadeDB, that:
// migrations apply (and are idempotent), the vector + pg_search extensions load,
// the core tables exist, and the org->project->run->event chain round-trips
// through the sqlc queries.
func TestMigrationsExtensionsAndRoundTrip(t *testing.T) {
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
		var exists bool
		if err := st.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`, tbl).Scan(&exists); err != nil {
			t.Fatalf("query table %s: %v", tbl, err)
		}
		if !exists {
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

	got, err := q.GetEvent(ctx, ev.ID)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if got.AgentID != "researcher" {
		t.Errorf("agent_id = %q, want %q", got.AgentID, "researcher")
	}

	count, err := q.CountEvents(ctx)
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
	// goose provider directly: Down reverts 0002 (the inline embedding column returns
	// and the added columns disappear) and Up restores it.
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB for goose: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("create goose provider: %v", err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("goose down (revert 0002): %v", err)
	}
	if !memoriesHasColumn(ctx, t, st.Pool, "embedding") {
		t.Error("down 0002 should restore memories.embedding")
	}
	if memoriesHasColumn(ctx, t, st.Pool, "entities") {
		t.Error("down 0002 should drop memories.entities")
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("goose up (reapply 0002): %v", err)
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
