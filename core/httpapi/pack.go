package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/pack"
	"github.com/lore-gpt/lore/core/retrieval"
)

// maxPackBody caps the pack request body: a query plus a few scopes, nothing large.
const maxPackBody = 1 << 16 // 64 KiB

// defaultPackLimit bounds the distilled memories retrieved when the request omits a limit.
const defaultPackLimit = 20

// PackRequest is the POST /v1/pack body. The project is taken from the API key, never the body — a key can
// only pack its own project's runs.
type PackRequest struct {
	RunID       string   `json:"run_id"`
	Query       string   `json:"query"`
	MinSeq      int64    `json:"min_seq"`
	Scopes      []string `json:"scopes"`
	Limit       int      `json:"limit"`
	TokenBudget int      `json:"token_budget"`
}

// PackResponse is the 200 body: the assembled context pack and its provenance.
type PackResponse struct {
	Text           string       `json:"text"`
	Sources        []PackSource `json:"sources"`
	CoveredSeq     int64        `json:"covered_seq"`
	FreshnessLagMs int64        `json:"freshness_lag_ms"`
	SavedTokens    int          `json:"saved_tokens"`
	WorkingSource  string       `json:"working_source"`
	Truncated      bool         `json:"truncated"`
}

// PackSource is one distilled memory that composed the pack, in pack order.
type PackSource struct {
	ID      string  `json:"id"`
	Kind    string  `json:"kind"`
	Score   float64 `json:"score"`
	Section string  `json:"section"`
}

// handlePack builds a context pack for a run within the API key's project. The project comes only from the
// authenticated key (never the body), so a key can only pack its own project's runs; a run in another project
// returns the same 404 an unknown run gets (no existence oracle). The build runs inside one tenant transaction
// so retrieval, the working-memory merge, the raw tail, and the trace row are all project-scoped and atomic.
func (a *API) handlePack(w http.ResponseWriter, r *http.Request) {
	if a.packer == nil || a.tenant == nil {
		writeError(w, r, http.StatusNotImplemented, "not_implemented", "the pack endpoint is not configured")
		return
	}
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, maxPackBody)
	var req PackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_body", "request body is not valid JSON")
		return
	}
	runID, err := uuid.Parse(req.RunID)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_run_id", "run_id must be a UUID")
		return
	}
	projectID, ok := projectIDFromContext(ctx)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "internal", "missing authenticated project")
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultPackLimit
	}

	var res pack.Result
	err = a.tenant.WithProject(ctx, projectID, func(tx pgx.Tx) error {
		var e error
		res, e = a.packer.Build(ctx, tx, projectID, pgtype.UUID{Bytes: runID, Valid: true}, pack.Request{
			Query:       req.Query,
			MinSeq:      req.MinSeq,
			Filters:     retrieval.Filters{Scopes: req.Scopes},
			Limit:       limit,
			TokenBudget: req.TokenBudget,
		})
		return e
	})
	if err != nil {
		writePackError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, packResponse(res))
}

// writePackError maps a pack build error to an HTTP response. An unknown or cross-project run is a 404
// not_found (the run's project is read under the key's scope, so a run the key cannot see returns no row); a
// min_seq past the run's latest seq is a 400; a project with no active embedding model cannot be recalled yet
// (409); anything else is a 500.
func writePackError(w http.ResponseWriter, r *http.Request, err error) {
	var oor *pack.MinSeqOutOfRangeError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, r, http.StatusNotFound, "not_found", "run_id does not exist")
	case errors.As(err, &oor):
		writeError(w, r, http.StatusBadRequest, "min_seq_out_of_range", oor.Error())
	case errors.Is(err, retrieval.ErrNoActiveModel):
		writeError(w, r, http.StatusConflict, "no_active_model", "the project has no active embedding model; recall is unavailable")
	default:
		writeError(w, r, http.StatusInternalServerError, "internal", "could not build pack")
	}
}

// packResponse maps the pack result to the JSON response shape (memory ids rendered as strings).
func packResponse(res pack.Result) PackResponse {
	sources := make([]PackSource, len(res.Sources))
	for i, s := range res.Sources {
		sources[i] = PackSource{
			ID:      uuid.UUID(s.ID.Bytes).String(),
			Kind:    s.Kind,
			Score:   s.Score,
			Section: s.Section,
		}
	}
	return PackResponse{
		Text:           res.Text,
		Sources:        sources,
		CoveredSeq:     res.CoveredSeq,
		FreshnessLagMs: res.FreshnessLagMs,
		SavedTokens:    res.SavedTokens,
		WorkingSource:  res.WorkingSource,
		Truncated:      res.Truncated,
	}
}

// notImplemented is the shared handler for routes whose contract exists but whose implementation lands in a
// later increment. It answers 501 (not the router's 404) so a client sees "not implemented yet", not "no such
// route" — the endpoint is real, just unfinished.
func (a *API) notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotImplemented, "not_implemented", "this endpoint is not implemented yet")
}
