//go:build integration

package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
)

const paradeDBImage = "paradedb/paradedb:0.24.2-pg17"

// startParadeDB boots a ParadeDB container with the app and River schemas
// migrated, and returns its DSN.
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
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("store migrations: %v", err)
	}
	return dsn
}

// TestNewServerComposesHealthz proves NewServer wires store + queue + httpapi
// into a working handler: /healthz reports both dependencies healthy once the
// River schema is migrated.
func TestNewServerComposesHealthz(t *testing.T) {
	ctx := context.Background()
	dsn := startParadeDB(ctx, t)

	srv, err := NewServer(ctx, Config{Addr: "127.0.0.1:0", DatabaseURL: dsn})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.Close)

	// Migrate the River schema on the server's own pool, then the queue probe
	// passes. Before this the handler still composes; queue would report error.
	if err := queue.Migrate(ctx, srv.store.Pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
}

// TestNewWorkerStartStop proves NewWorker composes a queue client that can start
// and stop cleanly — the structural counterpart to the insert-only server.
func TestNewWorkerStartStop(t *testing.T) {
	ctx := context.Background()
	dsn := startParadeDB(ctx, t)

	w, err := NewWorker(ctx, Config{DatabaseURL: dsn})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	t.Cleanup(w.Close)

	if err := queue.Migrate(ctx, w.store.Pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- w.Start(runCtx) }()

	time.Sleep(500 * time.Millisecond) // let the worker start
	cancel()                           // trigger graceful shutdown

	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("worker Start = %v, want nil after graceful stop", err)
		}
	case <-time.After(20 * time.Second):
		t.Error("worker did not stop within 20s")
	}
}
