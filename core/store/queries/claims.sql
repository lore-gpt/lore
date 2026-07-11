-- name: InsertClaim :one
INSERT INTO claims (memory_id, project_id, entity_id, predicate, value, event_time)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, memory_id, project_id, entity_id, predicate, value, event_time, superseded_by, created_at;

-- Supersede only a currently-active claim; the row count lets the caller detect a
-- no-op (the claim was already superseded), keeping the one-active-per-subject
-- invariant enforceable from the write path rather than from the index alone.
-- name: SupersedeClaim :execrows
UPDATE claims
SET superseded_by = $2
WHERE id = $1 AND superseded_by IS NULL;
