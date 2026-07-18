package httpapi

import "net/http"

// Health is the /healthz body. It mirrors the Health schema in
// spec/openapi.yaml.
type Health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	DB      string `json:"db"`
	Queue   string `json:"queue"`
	// Workmem is the working-memory stripe's mode: "ok", "degraded", or "disabled". It is reported for
	// visibility but never affects Status — the stripe is optional and Postgres stays authoritative, so a
	// degraded or absent cache must not fail the instance.
	Workmem string `json:"workmem"`
	// Embedder is the model@dim identity of the composed embedding model (e.g. "fixture-embed-v1@64" or
	// "text-embedding-3-small@1536"). It is reported so an operator can confirm the server and worker share
	// one vector space; it never affects Status.
	Embedder string `json:"embedder"`
}

// handleHealthz reports process version and dependency health. It returns 200
// when both the database and queue answer, and 503 when either is unreachable —
// so a container/orchestrator healthcheck fails the instance instead of routing
// traffic to a broken one. The healthy 200 body is the shape documented in the
// OpenAPI spec.
//
// TODO: this single endpoint conflates liveness with readiness. Under an
// orchestrator that restarts on liveness failure, a transient dependency outage
// here would trigger pointless restarts; split into /healthz (process up) and
// /readyz (dependencies ok) when we add that deployment target.
func (a *API) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h := Health{Status: "ok", Version: a.version, DB: "ok", Queue: "ok", Workmem: a.workmem.Mode().String(), Embedder: a.embedderID}

	if err := a.db.Ping(ctx); err != nil {
		h.DB = "error"
		h.Status = "degraded"
	}
	if err := a.queue.Ping(ctx); err != nil {
		h.Queue = "error"
		h.Status = "degraded"
	}

	status := http.StatusOK
	if h.Status != "ok" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, r, status, h)
}
