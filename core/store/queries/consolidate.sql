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
