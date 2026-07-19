package httpapi

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/store/db"
)

const (
	// defaultInspectLimit is the page size when a list request omits limit; maxInspectLimit caps it so a client
	// cannot ask for an unbounded page.
	defaultInspectLimit = 50
	maxInspectLimit     = 200
)

// Memory is the inspection view of one currently-valid memory.
type Memory struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	Content        string    `json:"content"`
	CreatedByAgent string    `json:"created_by_agent"`
	CreatedAt      time.Time `json:"created_at"`
	Version        int32     `json:"version"`
	TrustTier      string    `json:"trust_tier"`
	ReviewStatus   string    `json:"review_status"`
	ScopeKeys      []string  `json:"scope_keys"`
	SourceEventID  *string   `json:"source_event_id"`
}

// MemoryListResponse is the GET /v1/memories page. next_cursor is present only in browse mode when a further
// page exists; search mode paginates by limit alone.
type MemoryListResponse struct {
	Memories   []Memory `json:"memories"`
	HasMore    bool     `json:"has_more"`
	NextCursor *string  `json:"next_cursor,omitempty"`
}

// MemoryVersion is one entry of a memory's edit history.
type MemoryVersion struct {
	Version   int32     `json:"version"`
	Content   string    `json:"content"`
	ChangedBy *string   `json:"changed_by,omitempty"`
	Reason    *string   `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// MemoryVersionListResponse is the GET /v1/memories/{id}/versions body, oldest first.
type MemoryVersionListResponse struct {
	Versions []MemoryVersion `json:"versions"`
}

// RunTraceEntry is one pack built for a run.
type RunTraceEntry struct {
	ID             string    `json:"id"`
	CreatedAt      time.Time `json:"created_at"`
	Query          string    `json:"query"`
	CoveredSeq     *int64    `json:"covered_seq,omitempty"`
	FreshnessLagMs *int64    `json:"freshness_lag_ms,omitempty"`
	LatencyMs      *int64    `json:"latency_ms,omitempty"`
	MemoryIDs      []string  `json:"memory_ids"`
	PackHash       *string   `json:"pack_hash,omitempty"`
}

// RunTraceResponse is the GET /v1/runs/{id}/trace page, newest first.
type RunTraceResponse struct {
	Packs      []RunTraceEntry `json:"packs"`
	HasMore    bool            `json:"has_more"`
	NextCursor *string         `json:"next_cursor,omitempty"`
}

// handleListMemories lists (browse) or searches (?q, lexical) the API key's project memories. It reads only
// currently-valid rows and is scoped to the authenticated project; search rides the same lexical index the
// read path uses and needs no embedding model.
func (a *API) handleListMemories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID, ok := projectIDFromContext(ctx)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "internal", "missing authenticated project")
		return
	}
	limit, err := inspectLimit(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
		return
	}
	runID, err := queryUUID(r, "run_id")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_run_id", "run_id must be a UUID")
		return
	}
	kind := queryString(r, "kind")
	trustTier := queryString(r, "trust_tier")
	reviewStatus := queryString(r, "review_status")
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	var resp MemoryListResponse
	if q != "" {
		err = a.tenant.WithProject(ctx, projectID, func(tx pgx.Tx) error {
			rows, e := db.New(tx).ListMemoriesSearch(ctx, db.ListMemoriesSearchParams{
				ProjectID: projectID, QueryText: q, Kind: kind, TrustTier: trustTier,
				ReviewStatus: reviewStatus, RunID: runID, Lim: limit + 1,
			})
			if e != nil {
				return e
			}
			resp = searchListResponse(rows, int(limit))
			return nil
		})
	} else {
		cursorAt, cursorID, cerr := decodeCursor(r.URL.Query().Get("cursor"))
		if cerr != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_cursor", "cursor is not a valid pagination cursor")
			return
		}
		err = a.tenant.WithProject(ctx, projectID, func(tx pgx.Tx) error {
			rows, e := db.New(tx).ListMemoriesBrowse(ctx, db.ListMemoriesBrowseParams{
				ProjectID: projectID, Kind: kind, TrustTier: trustTier, ReviewStatus: reviewStatus,
				RunID: runID, CursorCreatedAt: cursorAt, CursorID: cursorID, Lim: limit + 1,
			})
			if e != nil {
				return e
			}
			resp = browseListResponse(rows, int(limit))
			return nil
		})
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "could not list memories")
		return
	}
	writeJSON(w, r, http.StatusOK, resp)
}

// handleGetMemory returns one currently-valid memory. A soft-deleted, superseded, unknown, or cross-project id
// is the same 404 (no existence oracle across projects).
func (a *API) handleGetMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID, ok := projectIDFromContext(ctx)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "internal", "missing authenticated project")
		return
	}
	id, err := pathUUID(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "id must be a UUID")
		return
	}
	var mem Memory
	found := false
	err = a.tenant.WithProject(ctx, projectID, func(tx pgx.Tx) error {
		row, e := db.New(tx).GetMemory(ctx, db.GetMemoryParams{ProjectID: projectID, ID: id})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		mem = toMemory(row.ID, row.Kind, row.Content, row.CreatedByAgent, row.CreatedAt,
			row.Version, row.TrustTier, row.ReviewStatus, row.ScopeKeys, row.SourceEventID)
		found = true
		return nil
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "could not read memory")
		return
	}
	if !found {
		writeError(w, r, http.StatusNotFound, "not_found", "no such memory in this project")
		return
	}
	writeJSON(w, r, http.StatusOK, mem)
}

// handleListMemoryVersions returns a memory's version history (oldest first). It serves history even for a
// soft-deleted memory (the row is retained), so it checks the row exists — including deleted/superseded — and
// 404s only a genuinely unknown or cross-project id.
func (a *API) handleListMemoryVersions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID, ok := projectIDFromContext(ctx)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "internal", "missing authenticated project")
		return
	}
	id, err := pathUUID(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "id must be a UUID")
		return
	}
	resp := MemoryVersionListResponse{Versions: []MemoryVersion{}}
	found := false
	err = a.tenant.WithProject(ctx, projectID, func(tx pgx.Tx) error {
		exists, e := db.New(tx).MemoryRowExists(ctx, db.MemoryRowExistsParams{ProjectID: projectID, ID: id})
		if e != nil {
			return e
		}
		if !exists {
			return nil
		}
		found = true
		rows, e := db.New(tx).ListMemoryVersions(ctx, db.ListMemoryVersionsParams{ProjectID: projectID, MemoryID: id})
		if e != nil {
			return e
		}
		resp = versionsResponse(rows)
		return nil
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "could not read memory versions")
		return
	}
	if !found {
		writeError(w, r, http.StatusNotFound, "not_found", "no such memory in this project")
		return
	}
	writeJSON(w, r, http.StatusOK, resp)
}

// handleDeleteMemory soft-deletes a memory (stamps valid_to) and appends an audit_log row, in one tenant
// transaction. A second delete of the same row, or an unknown/superseded/cross-project id, updates no row and
// is a 404. It does NOT clear a matching hot working-memory fact: that key is not derivable from the memory
// row, so a hot echo (if any) expires on its own TTL — a documented v0 boundary, tracked for the entity
// substrate that would make it derivable. Claims distilled from the memory keep their own lifecycle.
func (a *API) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID, ok := projectIDFromContext(ctx)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "internal", "missing authenticated project")
		return
	}
	id, err := pathUUID(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "id must be a UUID")
		return
	}
	deleted := false
	err = a.tenant.WithProject(ctx, projectID, func(tx pgx.Tx) error {
		q := db.New(tx)
		_, e := q.SoftDeleteMemory(ctx, db.SoftDeleteMemoryParams{ProjectID: projectID, ID: id})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil // no live row: already deleted, superseded, unknown, or another project's
		}
		if e != nil {
			return e
		}
		deleted = true
		target := uuid.UUID(id.Bytes).String()
		return q.InsertAuditLog(ctx, db.InsertAuditLogParams{
			ProjectID: projectID, Actor: "api", Action: "memory.delete", Target: &target, Detail: []byte("{}"),
		})
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "could not delete memory")
		return
	}
	if !deleted {
		writeError(w, r, http.StatusNotFound, "not_found", "no such memory in this project")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGetRunTrace returns a run's context-pack history (newest first), keyset-paginated. A run with no packs
// yet is a 200 empty page; an unknown or cross-project run is a 404.
func (a *API) handleGetRunTrace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID, ok := projectIDFromContext(ctx)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "internal", "missing authenticated project")
		return
	}
	runID, err := pathUUID(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "id must be a UUID")
		return
	}
	limit, err := inspectLimit(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
		return
	}
	cursorAt, cursorID, cerr := decodeCursor(r.URL.Query().Get("cursor"))
	if cerr != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_cursor", "cursor is not a valid pagination cursor")
		return
	}
	var resp RunTraceResponse
	found := false
	err = a.tenant.WithProject(ctx, projectID, func(tx pgx.Tx) error {
		exists, e := db.New(tx).RunRowExists(ctx, db.RunRowExistsParams{ProjectID: projectID, ID: runID})
		if e != nil {
			return e
		}
		if !exists {
			return nil
		}
		found = true
		rows, e := db.New(tx).ListRunPackLogs(ctx, db.ListRunPackLogsParams{
			ProjectID: projectID, RunID: runID, CursorCreatedAt: cursorAt, CursorID: cursorID, Lim: limit + 1,
		})
		if e != nil {
			return e
		}
		resp = traceResponse(rows, int(limit))
		return nil
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "could not read run trace")
		return
	}
	if !found {
		writeError(w, r, http.StatusNotFound, "not_found", "no such run in this project")
		return
	}
	writeJSON(w, r, http.StatusOK, resp)
}

// --- helpers ---

// inspectLimit reads the limit query param: absent → default, present-but-invalid → error, out of range →
// clamped to [1, maxInspectLimit].
func inspectLimit(r *http.Request) (int32, error) {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return defaultInspectLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, errors.New("invalid limit")
	}
	if n > maxInspectLimit {
		n = maxInspectLimit
	}
	return int32(n), nil
}

// queryString returns a pointer to a non-empty query param, or nil (the optional-filter shape sqlc.narg wants).
func queryString(r *http.Request, key string) *string {
	if s := r.URL.Query().Get(key); s != "" {
		return &s
	}
	return nil
}

// queryUUID parses an optional uuid query param: absent → invalid (nil) UUID, present-but-malformed → error.
func queryUUID(r *http.Request, key string) (pgtype.UUID, error) {
	s := r.URL.Query().Get(key)
	if s == "" {
		return pgtype.UUID{}, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: id, Valid: true}, nil
}

// pathUUID parses the {id} path parameter as a UUID.
func pathUUID(r *http.Request) (pgtype.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: id, Valid: true}, nil
}

// encodeCursor renders an opaque keyset cursor from a (created_at, id) pair.
func encodeCursor(t time.Time, id pgtype.UUID) string {
	raw := t.UTC().Format(time.RFC3339Nano) + "|" + uuid.UUID(id.Bytes).String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses an opaque cursor back to a (created_at, id) pair. An empty string is "no cursor" (the
// first page); a malformed one is an error.
func decodeCursor(s string) (pgtype.Timestamptz, pgtype.UUID, error) {
	if s == "" {
		return pgtype.Timestamptz{}, pgtype.UUID{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, err
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return pgtype.Timestamptz{}, pgtype.UUID{}, errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, err
	}
	return pgtype.Timestamptz{Time: t, Valid: true}, pgtype.UUID{Bytes: id, Valid: true}, nil
}

// toMemory maps the shared memory columns (returned identically by the browse, search, and get queries) to the
// JSON view. scope_keys is normalized to an empty array so it never serializes as null.
func toMemory(id pgtype.UUID, kind, content string, agent *string, createdAt pgtype.Timestamptz,
	version int32, trustTier, reviewStatus string, scopeKeys []string, sourceEventID pgtype.UUID) Memory {
	m := Memory{
		ID:             uuid.UUID(id.Bytes).String(),
		Kind:           kind,
		Content:        content,
		CreatedByAgent: strOrEmpty(agent),
		CreatedAt:      createdAt.Time,
		Version:        version,
		TrustTier:      trustTier,
		ReviewStatus:   reviewStatus,
		ScopeKeys:      scopeKeys,
	}
	if m.ScopeKeys == nil {
		m.ScopeKeys = []string{}
	}
	if sourceEventID.Valid {
		s := uuid.UUID(sourceEventID.Bytes).String()
		m.SourceEventID = &s
	}
	return m
}

// browseListResponse trims the over-fetched page, sets has_more, and derives the next keyset cursor.
func browseListResponse(rows []db.ListMemoriesBrowseRow, limit int) MemoryListResponse {
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	mems := make([]Memory, len(rows))
	for i, row := range rows {
		mems[i] = toMemory(row.ID, row.Kind, row.Content, row.CreatedByAgent, row.CreatedAt,
			row.Version, row.TrustTier, row.ReviewStatus, row.ScopeKeys, row.SourceEventID)
	}
	resp := MemoryListResponse{Memories: mems, HasMore: hasMore}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		c := encodeCursor(last.CreatedAt.Time, last.ID)
		resp.NextCursor = &c
	}
	return resp
}

// searchListResponse trims the over-fetched search page and sets has_more; search paginates by limit alone, so
// there is no keyset cursor.
func searchListResponse(rows []db.ListMemoriesSearchRow, limit int) MemoryListResponse {
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	mems := make([]Memory, len(rows))
	for i, row := range rows {
		mems[i] = toMemory(row.ID, row.Kind, row.Content, row.CreatedByAgent, row.CreatedAt,
			row.Version, row.TrustTier, row.ReviewStatus, row.ScopeKeys, row.SourceEventID)
	}
	return MemoryListResponse{Memories: mems, HasMore: hasMore}
}

// versionsResponse maps the version-history rows to the JSON view.
func versionsResponse(rows []db.MemoryVersion) MemoryVersionListResponse {
	versions := make([]MemoryVersion, len(rows))
	for i, row := range rows {
		versions[i] = MemoryVersion{
			Version:   row.Version,
			Content:   row.Content,
			ChangedBy: row.ChangedBy,
			Reason:    row.Reason,
			CreatedAt: row.CreatedAt.Time,
		}
	}
	return MemoryVersionListResponse{Versions: versions}
}

// traceResponse trims the over-fetched pack-trace page, sets has_more, and derives the next keyset cursor.
func traceResponse(rows []db.ListRunPackLogsRow, limit int) RunTraceResponse {
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	packs := make([]RunTraceEntry, len(rows))
	for i, row := range rows {
		ids := make([]string, len(row.MemoryIds))
		for j, mid := range row.MemoryIds {
			ids[j] = uuid.UUID(mid.Bytes).String()
		}
		e := RunTraceEntry{
			ID:             uuid.UUID(row.ID.Bytes).String(),
			CreatedAt:      row.CreatedAt.Time,
			Query:          row.Query,
			CoveredSeq:     row.CoveredSeq,
			FreshnessLagMs: widen(row.FreshnessLagMs),
			LatencyMs:      widen(row.LatencyMs),
			MemoryIDs:      ids,
		}
		if len(row.PackHash) > 0 {
			s := hex.EncodeToString(row.PackHash)
			e.PackHash = &s
		}
		packs[i] = e
	}
	resp := RunTraceResponse{Packs: packs, HasMore: hasMore}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		c := encodeCursor(last.CreatedAt.Time, last.ID)
		resp.NextCursor = &c
	}
	return resp
}

// strOrEmpty dereferences a nullable string column to "" when null.
func strOrEmpty(p *string) string {
	if p != nil {
		return *p
	}
	return ""
}

// widen converts a nullable int32 column to a nullable int64 for the JSON view.
func widen(p *int32) *int64 {
	if p == nil {
		return nil
	}
	v := int64(*p)
	return &v
}
