//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
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
}
