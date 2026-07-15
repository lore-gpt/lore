//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/httpapi"
	"github.com/lore-gpt/lore/core/pack"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/retrieval"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/workmem"
)

// packHandler builds the full HTTP handler with a real context-pack read path (hybrid retrieval over the
// offline fixture embedder), so /v1/pack exercises the actual composition — WithProject scoping, Build, and
// the pack_logs trace — end to end.
func packHandler(st *store.Store, q *queue.Queue) http.Handler {
	packer := pack.New(retrieval.NewHybrid(retrieval.New(), ext.FixtureEmbedder{}), workmem.NewDisabled())
	return httpapi.New(httpapi.Config{
		Pool: st.Pool, Enqueuer: q, DB: st, Queue: q, Packer: packer, Tenant: st, Version: "test",
	}).Handler()
}

func doPack(handler http.Handler, apiKey, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/pack", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func setActiveModel(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID, model string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE projects SET active_model_id = $2 WHERE id = $1`, projectID, model); err != nil {
		t.Fatalf("set active model: %v", err)
	}
}

// TestPackEndpoint proves the /v1/pack surface end to end: a project with an active model returns a 200 pack
// whose raw tail carries a not-yet-distilled write; the auth boundary holds (401 unauthenticated, 404 for a
// cross-project run — no oracle); a project with no active model is 409; and a stub route is 501.
func TestPackEndpoint(t *testing.T) {
	ctx := context.Background()
	st, q := newStateTestStore(ctx, t)
	handler := packHandler(st, q)

	// Project A: an active model, partitions, a run, an uncovered event (extraction has not run), and a key.
	projA, runA := seedProjectRun(ctx, t, st.Pool)
	if err := store.CreateProjectPartitions(ctx, st.Pool, mustUUID(t, projA)); err != nil {
		t.Fatalf("create partitions: %v", err)
	}
	setActiveModel(ctx, t, st.Pool, projA, "fixture-embed-v1")
	keyA, _ := provisionKey(ctx, t, st.Pool, projA)

	seq := postEvent(t, handler, keyA, `{"run_id":"`+runA+`","agent_id":"a","payload":{"note":"uncovered_write"}}`)

	// --- Happy path: 200 with the pack, the uncovered write visible in the raw tail. ---
	rr := doPack(handler, keyA, `{"run_id":"`+runA+`","query":"anything","min_seq":`+strconv.FormatInt(seq, 10)+`}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("pack status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	var resp httpapi.PackResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode pack: %v", err)
	}
	if resp.CoveredSeq != 0 {
		t.Errorf("covered_seq = %d, want 0 (extraction has not run)", resp.CoveredSeq)
	}
	if resp.WorkingSource != "durable" {
		t.Errorf("working_source = %q, want durable (workmem disabled)", resp.WorkingSource)
	}
	if !strings.Contains(resp.Text, "uncovered_write") {
		t.Errorf("pack text missing the not-yet-distilled write (raw tail):\n%s", resp.Text)
	}

	// --- Unauthenticated: 401. ---
	if rr := doPack(handler, "lore_sk_bogus", `{"run_id":"`+runA+`","query":"q"}`); rr.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated pack status = %d, want 401", rr.Code)
	}

	// --- Cross-project run: key A cannot pack project B's run — same 404 an unknown run gets. ---
	_, runB := seedProjectRun(ctx, t, st.Pool)
	assertErr(t, doPack(handler, keyA, `{"run_id":"`+runB+`","query":"q"}`), http.StatusNotFound, "not_found")

	// --- min_seq beyond the run's latest seq: 400 min_seq_out_of_range. ---
	assertErr(t, doPack(handler, keyA, `{"run_id":"`+runA+`","query":"q","min_seq":999}`), http.StatusBadRequest, "min_seq_out_of_range")

	// --- Fresh project: an event written but no consolidation yet (so no active model) still packs — the raw
	// tail serves it, so read-your-writes holds from the first event and there is NO 409 for the missing model. ---
	projC, runC := seedProjectRun(ctx, t, st.Pool)
	keyC, _ := provisionKey(ctx, t, st.Pool, projC)
	seqC := postEvent(t, handler, keyC, `{"run_id":"`+runC+`","agent_id":"a","payload":{"note":"predistill_write"}}`)
	rrC := doPack(handler, keyC, `{"run_id":"`+runC+`","query":"anything","min_seq":`+strconv.FormatInt(seqC, 10)+`}`)
	if rrC.Code != http.StatusOK {
		t.Fatalf("fresh-project pack status = %d, want 200 (raw tail serves RYW before any model is pinned; body %q)", rrC.Code, rrC.Body.String())
	}
	var respC httpapi.PackResponse
	if err := json.Unmarshal(rrC.Body.Bytes(), &respC); err != nil {
		t.Fatalf("decode fresh pack: %v", err)
	}
	if !strings.Contains(respC.Text, "predistill_write") {
		t.Errorf("fresh-project pack missing the pre-consolidation write in the raw tail:\n%s", respC.Text)
	}

	// --- Stubbed surfaces answer 501 (behind auth), not the router's 404 — spot-check several routes. ---
	for _, s := range []struct{ method, path string }{
		{http.MethodGet, "/v1/memories"},
		{http.MethodPost, "/v1/memories"},
		{http.MethodGet, "/v1/memories/" + runA + "/versions"},
		{http.MethodGet, "/v1/runs/" + runA + "/trace"},
		{http.MethodGet, "/v1/policies"},
	} {
		req := httptest.NewRequest(s.method, s.path, nil)
		req.Header.Set("Authorization", "Bearer "+keyA)
		stub := httptest.NewRecorder()
		handler.ServeHTTP(stub, req)
		if stub.Code != http.StatusNotImplemented {
			t.Errorf("%s %s status = %d, want 501", s.method, s.path, stub.Code)
		}
	}
}
