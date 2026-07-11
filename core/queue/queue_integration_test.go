//go:build integration

package queue_test

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
)

const paradeDBImage = "paradedb/paradedb:0.24.2-pg17"

// TestMigrateAndPing proves the River migrator applies (and is idempotent)
// against a real ParadeDB, and that the queue health probe reflects whether the
// River schema exists.
func TestMigrateAndPing(t *testing.T) {
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

	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	q, err := queue.New(st.Pool)
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}

	// Before the River migration the schema is absent, so Ping must fail.
	if err := q.Ping(ctx); err == nil {
		t.Error("Ping() before migrate = nil, want error")
	}

	for pass := 1; pass <= 2; pass++ {
		if err := queue.Migrate(ctx, st.Pool); err != nil {
			t.Fatalf("river migrate (pass %d): %v", pass, err)
		}
	}

	if err := q.Ping(ctx); err != nil {
		t.Errorf("Ping() after migrate = %v, want nil", err)
	}
}
