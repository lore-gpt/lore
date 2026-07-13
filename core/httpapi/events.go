package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
)

// maxEventBody caps the request body so a single event cannot exhaust memory.
const maxEventBody = 1 << 20 // 1 MiB

// workmemWriteTimeout bounds the post-commit hot-lane write so a cache that died since the last health
// probe costs one request a little latency (and flips the stripe to Degraded) rather than stalling it.
const workmemWriteTimeout = 200 * time.Millisecond

// CreateEventRequest is the POST /v1/events body (spec CreateEventRequest).
type CreateEventRequest struct {
	RunID   string          `json:"run_id"`
	AgentID string          `json:"agent_id"`
	Payload json.RawMessage `json:"payload"`
}

// CreateEventResponse is the 202 body: the id of the appended event and its
// server-assigned, per-run sequence number. Clients carry seq to assert
// read-your-writes on recall.
type CreateEventResponse struct {
	EventID string `json:"event_id"`
	Seq     int64  `json:"seq"`
}

// handleCreateEvent appends an event and enqueues its extraction job in a single
// transaction. The write is atomic: if the enqueue (or the commit) fails, the
// event row is rolled back and the client gets an error, never a persisted event
// with no job or a job with no event. The event is stamped with a monotonic
// per-run seq assigned inside that same transaction, so a rolled-back write
// consumes no sequence number; the seq is returned to the client.
func (a *API) handleCreateEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, maxEventBody)
	var req CreateEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_body", "request body is not valid JSON")
		return
	}

	runID, err := uuid.Parse(req.RunID)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_run_id", "run_id must be a UUID")
		return
	}
	if req.AgentID == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_agent_id", "agent_id is required")
		return
	}
	if !isJSONObject(req.Payload) {
		writeError(w, r, http.StatusBadRequest, "invalid_payload", "payload must be a JSON object")
		return
	}

	// A kind:"state" event carries one hot fact for working memory. Validate it loudly at the door so a
	// malformed or oversized fact is rejected here rather than silently corrupting the run's working set.
	// Any other payload is opaque and passes through unchanged.
	fact, ferr := workmem.ParseStateFact(req.Payload, a.workmemMaxValueBytes)
	stateFact := ferr == nil
	if ferr != nil && !errors.Is(ferr, workmem.ErrNotStateFact) {
		writeError(w, r, http.StatusBadRequest, "invalid_state_fact", ferr.Error())
		return
	}

	tx, err := a.pool.Begin(ctx)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "could not begin transaction")
		return
	}
	// Rollback is a no-op once Commit succeeds; on any early return it undoes the
	// event insert so the write stays all-or-nothing.
	defer func() { _ = tx.Rollback(ctx) }()

	event, err := db.New(tx).InsertEvent(ctx, db.InsertEventParams{
		RunID:   pgtype.UUID{Bytes: runID, Valid: true},
		AgentID: req.AgentID,
		Payload: req.Payload,
	})
	if err != nil {
		// The insert derives project_id from the run, so an unknown run_id matches no row and
		// returns pgx.ErrNoRows rather than a foreign-key violation.
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, r, http.StatusBadRequest, "unknown_run", "run_id does not exist")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal", "could not persist event")
		return
	}

	eventID := uuid.UUID(event.ID.Bytes).String()
	// Extraction is coalesced per run: enqueue by the event's project and run (unique per run), not
	// per event, so a burst of events collapses into one extraction pass.
	projectID := uuid.UUID(event.ProjectID.Bytes).String()
	if err := a.enqueuer.EnqueueExtract(ctx, tx, projectID, req.RunID); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "could not enqueue extraction")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "could not commit transaction")
		return
	}

	// Write a state fact through to the hot lane after the event is durable, so a same-run reader sees it
	// immediately — the synchronous half of read-your-writes. The order is deliberate: commit, then write
	// through, then respond. Writing through before commit could surface a value a failed commit rolls back
	// (a phantom); responding before the write-through would let a same-run reader race it. Only attempted
	// when the stripe is Healthy, so a disabled or degraded cache adds no latency. If the process crashes
	// after commit but before the write-through, the fact is missing from the hot lane yet safe in the
	// durable event log — the next extraction pass reconstructs it.
	if stateFact && a.workmem.Mode() == workmem.Healthy {
		a.writeThroughState(ctx, event, fact)
	}

	writeJSON(w, r, http.StatusAccepted, CreateEventResponse{EventID: eventID, Seq: event.Seq})
}

// writeThroughState best-effort writes a state fact to working memory, bounded by its own short timeout so
// a cache that died since the last health probe degrades quickly instead of stalling the request. A
// failure is logged with identifiers only — never the value — and swallowed: the durable event log is the
// backstop, so ingestion never fails on the hot lane.
func (a *API) writeThroughState(ctx context.Context, event db.Event, fact workmem.StateFact) {
	// Detach from the request context first: the event is already committed, so a client that disconnects
	// while awaiting the 202 must not cancel this best-effort write — a caller-side cancel says nothing about
	// cache reachability and would otherwise flip the shared stripe to Degraded for every concurrent run. The
	// own timeout still bounds (and degrades) a genuinely slow or dead cache.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workmemWriteTimeout)
	defer cancel()

	k := workmem.Key{
		ProjectID: uuid.UUID(event.ProjectID.Bytes).String(),
		RunID:     uuid.UUID(event.RunID.Bytes).String(),
		Entity:    fact.Entity,
		Predicate: fact.Predicate,
	}
	if err := a.workmem.Set(wctx, k, workmem.Value{Value: fact.Value, Seq: event.Seq, Agent: event.AgentID}); err != nil {
		slog.WarnContext(ctx, "workmem_writethrough_failed",
			slog.String("project_id", k.ProjectID),
			slog.String("run_id", k.RunID),
			slog.Int64("seq", event.Seq))
	}
}

// handleRecall is the Phase 1 read path. Phase 0 answers 501 so the route and
// its error contract exist without shipping a partial implementation.
func (a *API) handleRecall(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotImplemented, "not_implemented", "recall lands in Phase 1")
}

// isJSONObject reports whether raw is a JSON object (the payload contract). The
// decoder has already validated raw is well-formed JSON, so it is enough to find
// the first non-whitespace byte and check it opens an object.
func isJSONObject(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}
