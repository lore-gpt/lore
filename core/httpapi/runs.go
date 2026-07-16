package httpapi

import (
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/store/db"
)

// maxRunBody caps the create-run request body. The request carries no fields, so anything larger than a
// small object is malformed; the cap keeps an oversized body from being streamed at the server.
const maxRunBody = 1 << 10 // 1 KiB

// CreateRunResponse is the 201 body: the new run's id and its start time.
type CreateRunResponse struct {
	RunID     string    `json:"run_id"`
	CreatedAt time.Time `json:"created_at"`
}

// handleCreateRun creates a run in the API key's project. The project comes only from the authenticated
// key (never the body), so a key can only create runs in its own project: the insert carries that
// project_id and runs inside a project-scoped transaction, tenant-safe by construction. The request body
// is ignored — a run carries no client-supplied fields today.
func (a *API) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// The request has no body. Cap and drain whatever the client sent so an oversized body is rejected here
	// (as the other write handlers bound theirs) rather than streamed at the server.
	r.Body = http.MaxBytesReader(w, r.Body, maxRunBody)
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_body", "request body too large")
		return
	}
	projectID, ok := projectIDFromContext(ctx)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "internal", "missing authenticated project")
		return
	}

	var runID pgtype.UUID
	var startedAt pgtype.Timestamptz
	err := a.tenant.WithProject(ctx, projectID, func(tx pgx.Tx) error {
		row, e := db.New(tx).CreateRun(ctx, projectID)
		if e != nil {
			return e
		}
		runID = row.ID
		startedAt = row.StartedAt
		return nil
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "could not create run")
		return
	}

	writeJSON(w, r, http.StatusCreated, CreateRunResponse{
		RunID:     uuid.UUID(runID.Bytes).String(),
		CreatedAt: startedAt.Time,
	})
}
