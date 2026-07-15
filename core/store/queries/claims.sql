-- name: InsertClaim :exec
-- Insert one claim with an explicit id. The write path pre-generates the id so it can point a
-- superseded claim at this replacement before this row exists (see SupersedeActiveClaimBySubject);
-- the self-FK is deferred, so the pointer is validated at commit. memory_id is nullable — a standalone
-- claim (no co-produced memory) has none — while source_event_id carries provenance on every claim.
INSERT INTO claims (id, memory_id, project_id, entity_id, predicate, value, event_time, source_event_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: GetActiveClaimsByEntities :many
-- The currently-active claims for a set of entities in one project — one row per (entity, predicate) that
-- has one, each carrying its value and the provenance (run + per-run seq) of the event it was distilled
-- from. The write path fetches them in a single round-trip (rather than once per claim) and resolves
-- conflicts against an in-memory overlay it updates as it supersedes and inserts within the pass, so two
-- claims for the same subject in one pass still resolve last-write-wins — exactly as a per-claim re-read
-- would, since a transaction sees its own writes. A policy needs the stored value (e.g. FieldMerge combines
-- it with the incoming one) and the provenance feeds the recorded reason. Fetching by entity (not the exact
-- (entity, predicate) subject) keeps this to one array parameter; a pass touches few entities, and the
-- caller keys its overlay by the full subject, so the extra rows for other predicates of the same entity
-- are simply never looked up. The partial-unique index leads with (project_id, entity_id), so this probes
-- it. run_id/seq are NULL for a manual claim with no source event; the events join is project-scoped so it
-- stays within the tenant.
SELECT c.entity_id, c.predicate, c.id, c.value, e.run_id, e.seq
FROM claims c
LEFT JOIN events e ON e.id = c.source_event_id AND e.project_id = c.project_id
WHERE c.project_id = sqlc.arg(project_id)
  AND c.entity_id = ANY(sqlc.arg(entity_ids)::uuid[])
  AND c.superseded_by IS NULL;

-- name: SupersedeActiveClaimBySubject :execrows
-- Close the currently-active claim for a subject (project, entity, predicate) by pointing it at its
-- replacement and stamping the resolution reason on it (the superseded row is the one whose state
-- changed), so a new active claim for the same subject can be inserted without violating
-- claims_active_subject_key. At most one row matches (the partial-unique index guarantees it), so the
-- rowcount is 0 (first assertion) or 1 (a policy resolved a conflict). The replacement id need not
-- exist yet — superseded_by is DEFERRABLE, validated at commit once the caller inserts it next.
UPDATE claims
SET superseded_by = sqlc.arg(superseded_by),
    resolution_reason = sqlc.arg(resolution_reason)
WHERE project_id = sqlc.arg(project_id)
  AND entity_id = sqlc.arg(entity_id)
  AND predicate = sqlc.arg(predicate)
  AND superseded_by IS NULL;

-- name: SupersedeClaim :execrows
-- Supersede a specific claim by id (only if still active), scoped to its project. Distinct from
-- SupersedeActiveClaimBySubject, which closes whichever claim is active for a subject.
UPDATE claims
SET superseded_by = $2
WHERE id = $1 AND superseded_by IS NULL AND project_id = $3;
