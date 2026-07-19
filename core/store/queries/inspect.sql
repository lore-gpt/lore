-- Inspection read/soft-delete surface (GET /v1/memories[/{id}][/versions], DELETE /v1/memories/{id},
-- GET /v1/runs/{id}/trace). Every query is project-scoped for tenant isolation (the RLS belt is the second
-- line); the list/get/delete queries all filter to the currently-valid head (valid_to IS NULL AND
-- superseded_by IS NULL) so a soft-deleted or superseded memory is invisible to reads exactly as it is to
-- retrieval and packs.

-- name: GetMemory :one
-- One currently-valid memory by id. A soft-deleted, superseded, unknown, or cross-project id returns no row
-- (the handler maps that to the same 404, so a key cannot probe another project's memory ids).
SELECT id, kind, content, created_by_agent, created_at, version, trust_tier, review_status, scope_keys, source_event_id
FROM memories
WHERE project_id = sqlc.arg(project_id) AND id = sqlc.arg(id)
  AND valid_to IS NULL AND superseded_by IS NULL;

-- name: ListMemoriesBrowse :many
-- Browse mode: the project's currently-valid memories, newest first, with optional column filters and a
-- keyset cursor. The cursor is the (created_at, id) of the last row of the previous page; the row-value
-- comparison walks the (created_at DESC, id DESC) order without an offset scan. run_id narrows to memories
-- distilled from that run's events (a memory with no source event is excluded when the run filter is set).
-- The handler fetches lim = limit + 1 to learn whether a further page exists.
SELECT id, kind, content, created_by_agent, created_at, version, trust_tier, review_status, scope_keys, source_event_id
FROM memories m
WHERE m.project_id = sqlc.arg(project_id)
  AND m.superseded_by IS NULL AND m.valid_to IS NULL
  AND (sqlc.narg(kind)::text IS NULL OR m.kind = sqlc.narg(kind))
  AND (sqlc.narg(trust_tier)::text IS NULL OR m.trust_tier = sqlc.narg(trust_tier))
  AND (sqlc.narg(review_status)::text IS NULL OR m.review_status = sqlc.narg(review_status))
  AND (sqlc.narg(run_id)::uuid IS NULL OR m.source_event_id IN (
        SELECT e.id FROM events e WHERE e.project_id = sqlc.arg(project_id) AND e.run_id = sqlc.narg(run_id)))
  AND (sqlc.narg(cursor_created_at)::timestamptz IS NULL
       OR (m.created_at, m.id) < (sqlc.narg(cursor_created_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY m.created_at DESC, m.id DESC
LIMIT sqlc.arg(lim)::int;

-- name: ListMemoriesSearch :many
-- Search mode: the same filtered set, but matched and ranked by a lexical full-text query over content. It
-- reuses the identical to_tsvector('english', content) @@ websearch_to_tsquery('english', ...) predicate the
-- dense read path's lexical leg uses, so it rides the same expression GIN index (a drift from that expression
-- silently falls to a sequential scan). It needs NO embedding model — search works on a deployment that never
-- pinned one. Ranked by lexical relevance with an id tie-break for a stable order; the handler fetches
-- lim = limit + 1 for the has_more signal. Search returns the first page only (no keyset cursor in v0).
SELECT id, kind, content, created_by_agent, created_at, version, trust_tier, review_status, scope_keys, source_event_id,
       ts_rank_cd(to_tsvector('english', m.content), websearch_to_tsquery('english', sqlc.arg(query_text)::text))::float8 AS rank
FROM memories m
WHERE m.project_id = sqlc.arg(project_id)
  AND m.superseded_by IS NULL AND m.valid_to IS NULL
  AND (sqlc.narg(kind)::text IS NULL OR m.kind = sqlc.narg(kind))
  AND (sqlc.narg(trust_tier)::text IS NULL OR m.trust_tier = sqlc.narg(trust_tier))
  AND (sqlc.narg(review_status)::text IS NULL OR m.review_status = sqlc.narg(review_status))
  AND (sqlc.narg(run_id)::uuid IS NULL OR m.source_event_id IN (
        SELECT e.id FROM events e WHERE e.project_id = sqlc.arg(project_id) AND e.run_id = sqlc.narg(run_id)))
  AND to_tsvector('english', m.content) @@ websearch_to_tsquery('english', sqlc.arg(query_text)::text)
ORDER BY rank DESC, m.id ASC
LIMIT sqlc.arg(lim)::int;

-- name: SoftDeleteMemory :one
-- Soft-delete the currently-valid memory: stamp its validity window closed so it drops out of every read
-- (retrieval, packs, list) that filters valid_to IS NULL, while the row and its version history are retained.
-- Idempotent per live row: a second delete (or an unknown/superseded/cross-project id) updates no row, which
-- the handler maps to a 404.
UPDATE memories SET valid_to = now()
WHERE project_id = sqlc.arg(project_id) AND id = sqlc.arg(id)
  AND valid_to IS NULL AND superseded_by IS NULL
RETURNING id;

-- name: InsertAuditLog :exec
-- Append one audit_log row. The table is INSERT-only (a trigger rejects UPDATE/DELETE), so this records an
-- immutable trail entry. project_id scopes it to the tenant; actor/action/target/detail record who did what to
-- which object (e.g. actor=api, action=memory.delete, target=<memory id>).
INSERT INTO audit_log (project_id, actor, action, target, detail)
VALUES (sqlc.arg(project_id), sqlc.arg(actor), sqlc.arg(action), sqlc.arg(target), sqlc.arg(detail));

-- name: MemoryRowExists :one
-- Whether a memory row exists in the project at all — INCLUDING soft-deleted and superseded rows — so the
-- version-history endpoint can 404 an unknown id while still serving a deleted memory's history.
SELECT EXISTS(SELECT 1 FROM memories WHERE project_id = sqlc.arg(project_id) AND id = sqlc.arg(id));

-- name: RunRowExists :one
-- Whether a run exists in the project, so the run-trace endpoint can 404 an unknown run (a real run with no
-- packs yet is a 200 empty page, not a 404).
SELECT EXISTS(SELECT 1 FROM runs WHERE project_id = sqlc.arg(project_id) AND id = sqlc.arg(id));

-- name: ListRunPackLogs :many
-- A run's context-pack history, newest first, keyset-paginated on (created_at, id). Rides the
-- pack_logs (project_id, created_at) index. pack_hash and the coverage/timing columns are nullable and passed
-- through as-is. The handler fetches lim = limit + 1 for the has_more signal.
SELECT id, query, covered_seq, freshness_lag_ms, latency_ms, memory_ids, pack_hash, created_at
FROM pack_logs
WHERE project_id = sqlc.arg(project_id) AND run_id = sqlc.arg(run_id)
  AND (sqlc.narg(cursor_created_at)::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg(cursor_created_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(lim)::int;
