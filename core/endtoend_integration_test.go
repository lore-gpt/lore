//go:build integration

package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lore-gpt/lore/core/apikey"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store/db"
)

// TestEventProcessedEndToEnd is the acceptance test for criterion 2: an event
// posted to the server is picked up and completed by the worker within 5s. It
// wires a real server (insert-only) and worker (working) against one ParadeDB,
// posts through the server's handler, and polls the job to completion.
func TestEventProcessedEndToEnd(t *testing.T) {
	ctx := context.Background()
	dsn := startParadeDB(ctx, t)

	srv, err := NewServer(ctx, Config{Addr: "127.0.0.1:0", DatabaseURL: dsn})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.Close)

	if err := queue.Migrate(ctx, srv.store.Pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}

	worker, err := NewWorker(ctx, Config{DatabaseURL: dsn})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	t.Cleanup(worker.Close)

	// Run the worker for the duration of the test.
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Start(runCtx) }()

	// Post an event through the composed HTTP handler, authenticated by a real minted key for the project.
	projectID, runID := seedRun(ctx, t, srv.store.Pool)
	token := provisionKey(ctx, t, srv.store.Pool, projectID)
	body := `{"run_id":"` + runID + `","agent_id":"researcher","payload":{"k":"v"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST /v1/events = %d, want 202 (body %q)", rr.Code, rr.Body.String())
	}

	// The stub worker completes the job; assert that happens within 5s.
	waitForCompletedJob(ctx, t, srv.store.Pool, 5*time.Second)

	// Graceful shutdown returns cleanly.
	cancel()
	select {
	case err := <-workerDone:
		if err != nil {
			t.Errorf("worker Start = %v, want nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Error("worker did not stop within 20s")
	}
}

// waitForCompletedJob polls until one river_job reaches the completed state or
// the timeout elapses.
func waitForCompletedJob(ctx context.Context, t *testing.T, pool *pgxpool.Pool, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for {
		var n int64
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE state = 'completed'").Scan(&n); err != nil {
			t.Fatalf("count completed jobs: %v", err)
		}
		if n >= 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("no completed job within %s", timeout)
		case <-tick.C:
		}
	}
}

// seedRun creates the org -> project -> run chain an event needs and returns the project and run ids as
// canonical UUID strings.
func seedRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (projectID, runID string) {
	t.Helper()
	q := db.New(pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "demo"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return uuid.UUID(proj.ID.Bytes).String(), uuid.UUID(run.ID.Bytes).String()
}

// provisionKey mints an API key for a project (as `lore keys create` does) and returns the raw bearer token.
func provisionKey(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID string) string {
	t.Helper()
	pid, err := uuid.Parse(projectID)
	if err != nil {
		t.Fatalf("parse project id: %v", err)
	}
	token, hash, prefix, err := apikey.New()
	if err != nil {
		t.Fatalf("mint key: %v", err)
	}
	if _, err := db.New(pool).CreateAPIKey(ctx, db.CreateAPIKeyParams{
		ProjectID: pgtype.UUID{Bytes: pid, Valid: true},
		KeyPrefix: &prefix,
		KeyHash:   hash,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return token
}
