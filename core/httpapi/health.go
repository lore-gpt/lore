package httpapi

import "net/http"

// Health is the /healthz body. It mirrors the Health schema in
// spec/openapi.yaml.
type Health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	DB      string `json:"db"`
	Queue   string `json:"queue"`
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
	h := Health{Status: "ok", Version: a.version, DB: "ok", Queue: "ok"}

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
