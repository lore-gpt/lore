package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/pack"
	"github.com/lore-gpt/lore/core/retrieval"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
)

// fakePinger is a Pinger whose health is fixed at construction.
type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// stubWorkmem is a workmem.Store with a fixed Mode, for asserting how /healthz reports the stripe and that
// its mode never changes the overall status. Set records the last write so a handler test can read it back.
type stubWorkmem struct {
	mode   workmem.Mode
	set    *workmem.Value // last Set value, if a pointer is provided
	key    *workmem.Key
	err    error  // returned by Set to exercise the write-through failure path
	ctxErr *error // if set, records ctx.Err() seen inside Set (to assert caller-cancellation detaching)
}

func (s stubWorkmem) Set(ctx context.Context, k workmem.Key, v workmem.Value) error {
	if s.ctxErr != nil {
		*s.ctxErr = ctx.Err()
	}
	if s.err != nil {
		return s.err
	}
	if s.set != nil {
		*s.set = v
		*s.key = k
	}
	return nil
}
func (stubWorkmem) Get(context.Context, workmem.Key) (workmem.Value, bool, error) {
	return workmem.Value{}, false, nil
}
func (stubWorkmem) GetAll(context.Context, string, string) ([]workmem.Entry, error) { return nil, nil }
func (s stubWorkmem) Mode() workmem.Mode                                            { return s.mode }
func (stubWorkmem) Close()                                                          {}

// fakeEnqueuer records nothing; it is only used where auth/validation reject the
// request before the enqueue would run.
type fakeEnqueuer struct{}

func (fakeEnqueuer) EnqueueExtract(context.Context, pgx.Tx, string, string) error { return nil }

// TestRequireAuthRejectsMalformedToken locks the parse half of the bearer-auth contract without a database: a
// missing header, a wrong scheme, or an empty token is rejected 401 BEFORE any api_keys lookup. The lookup half
// (an unknown or revoked key is 401, a valid key is accepted and scopes the request) needs a real database and
// is covered by the integration test.
func TestRequireAuthRejectsMalformedToken(t *testing.T) {
	handler := New(Config{}).Handler() // no pool: these reject before the lookup would run
	cases := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"wrong scheme", "Token s3cret"},
		{"empty token", "Bearer "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Probe via /v1/pack (behind requireAuth): a malformed token is rejected before the handler runs.
			req := httptest.NewRequest(http.MethodPost, "/v1/pack", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rr.Code)
			}
		})
	}
}

// TestHealthz checks the /healthz status code and body for both healthy and
// degraded dependencies. It needs no database: health is derived from Pingers.
func TestHealthz(t *testing.T) {
	boom := fakePinger{err: context.DeadlineExceeded}
	ok := fakePinger{}

	cases := []struct {
		name       string
		db, queue  Pinger
		workmem    workmem.Store // nil coerces to disabled
		wantStatus int
		wantBody   Health
	}{
		{"all ok, workmem disabled", ok, ok, nil, http.StatusOK, Health{Status: "ok", Version: "v-test", DB: "ok", Queue: "ok", Workmem: "disabled"}},
		{"db down", boom, ok, nil, http.StatusServiceUnavailable, Health{Status: "degraded", Version: "v-test", DB: "error", Queue: "ok", Workmem: "disabled"}},
		{"queue down", ok, boom, nil, http.StatusServiceUnavailable, Health{Status: "degraded", Version: "v-test", DB: "ok", Queue: "error", Workmem: "disabled"}},
		// The stripe's mode is reported but never drives overall status: a healthy or degraded cache both
		// leave status ok and the code 200 — only DB and Queue can fail the instance.
		{"workmem healthy, status still ok", ok, ok, stubWorkmem{mode: workmem.Healthy}, http.StatusOK, Health{Status: "ok", Version: "v-test", DB: "ok", Queue: "ok", Workmem: "ok"}},
		{"workmem degraded, status still ok", ok, ok, stubWorkmem{mode: workmem.Degraded}, http.StatusOK, Health{Status: "ok", Version: "v-test", DB: "ok", Queue: "ok", Workmem: "degraded"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := New(Config{DB: tc.db, Queue: tc.queue, Version: "v-test", Workmem: tc.workmem}).Handler()
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantStatus)
			}
			var got Health
			if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if got != tc.wantBody {
				t.Errorf("body = %+v, want %+v", got, tc.wantBody)
			}
		})
	}
}

// TestHealthzReportsEmbedderID checks that the composed embedder's model@dim
// identity is surfaced in the body, so an operator can confirm the server and
// worker share one vector space.
func TestHealthzReportsEmbedderID(t *testing.T) {
	ok := fakePinger{}
	handler := New(Config{DB: ok, Queue: ok, Version: "v-test", EmbedderID: "text-embedding-3-small@1536"}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	var got Health
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Embedder != "text-embedding-3-small@1536" {
		t.Errorf("embedder = %q, want the composed model@dim identity", got.Embedder)
	}
}

// TestCreateEventValidation covers the 400 paths that reject before any database
// work, so it runs without a pool. A malformed kind:"state" payload is rejected
// here too (invalid_state_fact), before the event is written. The happy path and
// the rollback guarantee are covered by the integration test.
func TestCreateEventValidation(t *testing.T) {
	// Call the handler directly (not through the router): these bodies are rejected before any auth lookup or
	// database work, so the test needs neither a key nor a pool.
	a := New(Config{Enqueuer: fakeEnqueuer{}})

	const runID = "11111111-1111-1111-1111-111111111111"
	state := func(payload string) string {
		return `{"run_id":"` + runID + `","agent_id":"a","payload":` + payload + `}`
	}
	cases := []struct {
		name     string
		body     string
		wantCode string // asserted when non-empty
	}{
		{name: "malformed json", body: "{"},
		{name: "missing run_id", body: `{"agent_id":"a","payload":{}}`},
		{name: "bad run_id", body: `{"run_id":"not-a-uuid","agent_id":"a","payload":{}}`},
		{name: "missing agent_id", body: `{"run_id":"` + runID + `","payload":{}}`},
		{name: "payload not object", body: `{"run_id":"` + runID + `","agent_id":"a","payload":[1,2]}`},
		{name: "payload missing", body: `{"run_id":"` + runID + `","agent_id":"a"}`},
		// A malformed state fact is rejected loudly at the door with a stable code.
		{name: "state missing entity", body: state(`{"kind":"state","predicate":"p","value":1}`), wantCode: "invalid_state_fact"},
		{name: "state missing value", body: state(`{"kind":"state","entity":"e","predicate":"p"}`), wantCode: "invalid_state_fact"},
		{name: "state control char in entity", body: state(`{"kind":"state","entity":"a\tb","predicate":"p","value":1}`), wantCode: "invalid_state_fact"},
		{name: "state entity not a string", body: state(`{"kind":"state","entity":5,"predicate":"p","value":1}`), wantCode: "invalid_state_fact"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			a.handleCreateEvent(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rr.Code, tc.body)
			}
			if tc.wantCode != "" {
				var e Error
				if err := json.NewDecoder(rr.Body).Decode(&e); err != nil {
					t.Fatalf("decode error body: %v", err)
				}
				if e.Code != tc.wantCode {
					t.Errorf("error code = %q, want %q (body %q)", e.Code, tc.wantCode, rr.Body.String())
				}
			}
		})
	}
}

// TestWriteThroughStateDetachesFromCallerCancellation proves a client that disconnects (cancelling the
// request context) does not cancel the post-commit best-effort write-through. Otherwise one caller's abort
// would surface as a Set error and poison the shared stripe health for every concurrent run.
func TestWriteThroughStateDetachesFromCallerCancellation(t *testing.T) {
	var sawErr error
	a := New(Config{Workmem: stubWorkmem{mode: workmem.Healthy, ctxErr: &sawErr}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client has already disconnected before the write-through runs

	event := db.Event{
		ProjectID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		RunID:     pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
		Seq:       1,
		AgentID:   "planner",
	}
	a.writeThroughState(ctx, event, workmem.StateFact{Entity: "e", Predicate: "p", Value: []byte(`"v"`)})

	if sawErr != nil {
		t.Errorf("write-through Set saw ctx.Err() = %v, want nil — a caller cancellation must be detached", sawErr)
	}
}

// TestCreateEventStateValueLimit proves the configurable value cap is enforced at ingestion: a value over
// the limit is rejected with invalid_state_fact before any database work.
func TestCreateEventStateValueLimit(t *testing.T) {
	a := New(Config{Enqueuer: fakeEnqueuer{}, WorkmemMaxValueBytes: 4})

	const runID = "11111111-1111-1111-1111-111111111111"
	body := `{"run_id":"` + runID + `","agent_id":"a","payload":{"kind":"state","entity":"e","predicate":"p","value":"way too long"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	rr := httptest.NewRecorder()
	a.handleCreateEvent(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	var e Error
	if err := json.NewDecoder(rr.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if e.Code != "invalid_state_fact" {
		t.Errorf("error code = %q, want invalid_state_fact", e.Code)
	}
}

// fakePacker records the arguments it was called with and returns a fixed result/error, so the pack handler's
// wiring (project source, request threading) and error mapping can be tested without a database.
type fakePacker struct {
	result     pack.Result
	err        error
	gotProject pgtype.UUID
	gotRun     pgtype.UUID
	gotReq     pack.Request
}

func (f *fakePacker) Build(_ context.Context, _ pgx.Tx, projectID, runID pgtype.UUID, req pack.Request) (pack.Result, error) {
	f.gotProject, f.gotRun, f.gotReq = projectID, runID, req
	return f.result, f.err
}

// fakeTenant runs the handler's closure with a nil transaction (the fake packer ignores it), recording the
// project it was scoped to.
type fakeTenant struct{ project pgtype.UUID }

func (t *fakeTenant) WithProject(_ context.Context, projectID pgtype.UUID, fn func(pgx.Tx) error) error {
	t.project = projectID
	return fn(nil)
}

// withProjectReq builds a /v1/pack request carrying an already-authenticated project in its context (the
// requireAuth stash), so a handler test can bypass the database key lookup.
func withProjectReq(body string, project pgtype.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/pack", strings.NewReader(body))
	return req.WithContext(withProjectID(req.Context(), project))
}

// TestHandlePackErrorMapping proves each pack build error becomes the right HTTP status and code.
func TestHandlePackErrorMapping(t *testing.T) {
	const runID = "11111111-1111-1111-1111-111111111111"
	project := pgtype.UUID{Bytes: [16]byte{7}, Valid: true}
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"unknown or cross-project run", pgx.ErrNoRows, http.StatusNotFound, "not_found"},
		{"min_seq out of range", &pack.MinSeqOutOfRangeError{MinSeq: 9, LastSeq: 3}, http.StatusBadRequest, "min_seq_out_of_range"},
		{"model mismatch", retrieval.ErrModelMismatch, http.StatusConflict, "model_mismatch"},
		{"unexpected", errors.New("boom"), http.StatusInternalServerError, "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := New(Config{Packer: &fakePacker{err: tc.err}, Tenant: &fakeTenant{}})
			rr := httptest.NewRecorder()
			a.handlePack(rr, withProjectReq(`{"run_id":"`+runID+`","query":"q"}`, project))
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			var e Error
			if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if e.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", e.Code, tc.wantCode)
			}
		})
	}
}

// TestHandlePackResponseAndProjectSource proves the response maps every pack field, and that the project comes
// ONLY from the authenticated context — never the request body — while the request's query/min_seq/scopes/
// limit/budget thread through to Build.
func TestHandlePackResponseAndProjectSource(t *testing.T) {
	project := pgtype.UUID{Bytes: [16]byte{7}, Valid: true}
	memID := pgtype.UUID{Bytes: [16]byte{9}, Valid: true}
	fp := &fakePacker{result: pack.Result{
		Text: "PACK TEXT", CoveredSeq: 5, FreshnessLagMs: 250, SavedTokens: 12,
		WorkingSource: "live", Truncated: true,
		Sources: []pack.Source{{ID: memID, Kind: "semantic", Score: 0.03, Section: "semantic"}},
	}}
	a := New(Config{Packer: fp, Tenant: &fakeTenant{}})

	body := `{"run_id":"22222222-2222-2222-2222-222222222222","query":"auth","min_seq":3,"scopes":["run:r1"],"limit":7,"token_budget":100}`
	rr := httptest.NewRecorder()
	a.handlePack(rr, withProjectReq(body, project))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	var resp PackResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Text != "PACK TEXT" || resp.CoveredSeq != 5 || resp.FreshnessLagMs != 250 ||
		resp.SavedTokens != 12 || resp.WorkingSource != "live" || !resp.Truncated {
		t.Errorf("response scalar fields wrong: %+v", resp)
	}
	if len(resp.Sources) != 1 || resp.Sources[0].ID != uuid.UUID(memID.Bytes).String() ||
		resp.Sources[0].Section != "semantic" || resp.Sources[0].Kind != "semantic" {
		t.Errorf("sources wrong: %+v", resp.Sources)
	}
	// The project is the auth-context one, never a body field.
	if fp.gotProject != project {
		t.Errorf("Build got project %v, want the auth-context project %v", fp.gotProject.Bytes, project.Bytes)
	}
	if fp.gotReq.MinSeq != 3 || fp.gotReq.Limit != 7 || fp.gotReq.TokenBudget != 100 ||
		len(fp.gotReq.Filters.Scopes) != 1 || fp.gotReq.Filters.Scopes[0] != "run:r1" || fp.gotReq.Query != "auth" {
		t.Errorf("Build got req %+v, want query auth / min_seq 3 / limit 7 / budget 100 / scopes [run:r1]", fp.gotReq)
	}
}

// TestHandlePackValidationAndUnconfigured proves the pre-build rejections and the unconfigured 501.
func TestHandlePackValidationAndUnconfigured(t *testing.T) {
	project := pgtype.UUID{Bytes: [16]byte{7}, Valid: true}

	a := New(Config{Packer: &fakePacker{}, Tenant: &fakeTenant{}})
	for _, tc := range []struct{ name, body, wantCode string }{
		{"malformed json", "{", "invalid_body"},
		{"bad run_id", `{"run_id":"nope","query":"q"}`, "invalid_run_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			a.handlePack(rr, withProjectReq(tc.body, project))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rr.Code, rr.Body.String())
			}
			var e Error
			_ = json.Unmarshal(rr.Body.Bytes(), &e)
			if e.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", e.Code, tc.wantCode)
			}
		})
	}

	// A nil Packer OR a nil Tenant independently leaves /v1/pack a 501 (never a panic on a nil dependency) —
	// tested separately so a mutant dropping either guard is caught.
	const packBody = `{"run_id":"22222222-2222-2222-2222-222222222222","query":"q"}`
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"nil packer", Config{Tenant: &fakeTenant{}}},
		{"nil tenant", Config{Packer: &fakePacker{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			New(tc.cfg).handlePack(rr, withProjectReq(packBody, project))
			if rr.Code != http.StatusNotImplemented {
				t.Errorf("status = %d, want 501", rr.Code)
			}
		})
	}
}

// TestHandlePackDefaultsLimit proves an omitted (non-positive) limit is defaulted, not threaded through as 0.
func TestHandlePackDefaultsLimit(t *testing.T) {
	fp := &fakePacker{}
	a := New(Config{Packer: fp, Tenant: &fakeTenant{}})
	rr := httptest.NewRecorder()
	a.handlePack(rr, withProjectReq(`{"run_id":"22222222-2222-2222-2222-222222222222","query":"q"}`,
		pgtype.UUID{Bytes: [16]byte{7}, Valid: true}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	if fp.gotReq.Limit != defaultPackLimit {
		t.Errorf("Build got limit %d, want the default %d", fp.gotReq.Limit, defaultPackLimit)
	}
}

// TestNotImplemented proves the shared stub handler answers 501 with a stable code.
func TestNotImplemented(t *testing.T) {
	rr := httptest.NewRecorder()
	New(Config{}).notImplemented(rr, httptest.NewRequest(http.MethodGet, "/v1/memories", nil))
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
	var e Error
	_ = json.Unmarshal(rr.Body.Bytes(), &e)
	if e.Code != "not_implemented" {
		t.Errorf("code = %q, want not_implemented", e.Code)
	}
}
