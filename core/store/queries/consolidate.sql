-- name: AcquireEntityLocks :exec
-- Take a transaction-scoped advisory lock for each of the given entity natural keys, all at once and in
-- a deterministic order, so two consolidation passes that touch a shared entity serialise instead of
-- racing, while passes over disjoint entities still run in parallel. Two properties matter:
--
--   * The key is hashed from (project_id, entity_name) — the natural key, NOT the row id — because the
--     race to create an entity and write its claim must serialise before any id exists; an id-based key
--     could not lock a not-yet-created row. project_id scopes the key so equal names in different
--     tenants do not collide.
--   * Acquisition is ordered by the hashed key (the ORDER BY drives the order the locks are taken),
--     independent of the caller's input order, so two passes can never deadlock by taking the same two
--     locks in opposite orders. The caller passes the whole entity set once, up front, before any write,
--     so no lock is taken mid-transaction.
--
-- Locks release automatically when the transaction ends (commit or rollback). Distinct names that hash
-- to the same key serialise unnecessarily — a rare, accepted cost that never yields an incorrect result.
-- An empty entity set takes no locks.
SELECT pg_advisory_xact_lock(k)
FROM (
    SELECT DISTINCT hashtextextended((sqlc.arg(project_id)::uuid)::text || ':' || name, 42) AS k
    FROM unnest(sqlc.arg(entity_names)::text[]) AS name
    ORDER BY k
) locks;

-- name: FindNearestLiveMemoryInBucket :one
-- The live memory most similar (by embedding cosine distance) to a candidate, within the candidate's
-- entity bucket (same context_hash) and single model space. It is the near-duplicate probe the
-- consolidation path runs after an exact-fingerprint miss: the caller merges into the returned memory when
-- the distance is below the merge threshold, records telemetry when it is in the grey zone, and inserts a
-- fresh memory otherwise. Scoping to the context_hash bucket (an index range-scan over live rows) is the
-- blocking step that avoids a whole-project O(N) comparison; the bucket is further capped so a large
-- bucket never runs an unbounded number of comparisons — bucket_size reports the full (pre-cap) live
-- bucket count so the caller can warn when the cap was hit and candidates beyond it went uncompared. No
-- live bucket member returns pgx.ErrNoRows (the caller's "insert fresh" signal). The distance operator is
-- cosine (the embeddings opclass), so it is a small exact scan over the bucket, not an ANN index probe.
WITH bucket AS (
    SELECT m.id, e.vec,
           count(*) OVER () AS bucket_size
    FROM memories m
    JOIN embeddings e ON e.project_id = m.project_id AND e.memory_id = m.id
    WHERE m.project_id = sqlc.arg(project_id)
      AND e.project_id = sqlc.arg(project_id)
      AND m.context_hash = sqlc.arg(context_hash)
      AND m.superseded_by IS NULL AND m.valid_to IS NULL
      AND e.model_id = sqlc.arg(model_id)
    ORDER BY m.created_at DESC
    LIMIT sqlc.arg(scan_cap)::int
)
SELECT id, bucket_size,
       (vec <=> sqlc.arg(query_vec)::vector)::float8 AS distance
FROM bucket
ORDER BY distance ASC
LIMIT 1;
