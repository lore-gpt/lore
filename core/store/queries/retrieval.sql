-- name: GetActiveModelID :one
-- The project's active embedding model — the single model space a read queries. NULL until a model is
-- chosen; the retriever treats that as a loud, typed error, never a silent empty result. Scoped by the
-- project's own id, which is the tenant root's RLS subject (projects is scoped by id, not project_id).
-- lore:tenant-exempt: projects is the tenant root; it is scoped by its own id (the RLS subject), not project_id
SELECT active_model_id FROM projects WHERE id = $1;

-- name: CountRetrievalCandidates :one
-- Count the live memories matching the retrieval filters, BOUNDED to the crossover threshold + 1: the
-- retriever only needs to know whether the filtered set is small (exact scan) or large (index-backed), so
-- it counts at most scan_limit rows via the inner LIMIT, never a full COUNT(*) over a large partition.
-- Empty scopes means project-wide; include_quarantine=false (the default) excludes quarantine-tier
-- memories. The filter shape is the compiled-ACL slot: L2 changes only where scopes comes from, not the
-- && overlap. Tenant partition = project_id (on both tables); scope_keys && uses the GIN index.
SELECT count(*) FROM (
    SELECT 1
    FROM memories m
    JOIN embeddings e ON e.project_id = m.project_id AND e.memory_id = m.id
    WHERE m.project_id = sqlc.arg(project_id) AND e.project_id = sqlc.arg(project_id)
      AND e.model_id = sqlc.arg(model_id)
      AND m.superseded_by IS NULL AND m.valid_to IS NULL
      AND (cardinality(sqlc.arg(scopes)::text[]) = 0 OR m.scope_keys && sqlc.arg(scopes)::text[])
      AND (sqlc.arg(include_quarantine)::bool OR m.trust_tier <> 'quarantine')
    LIMIT sqlc.arg(scan_limit)::int
) t;

-- name: RetrieveExact :many
-- The exact-scan retrieval path: the filtered live memories nearest the query vector by cosine distance,
-- used when the filtered candidate set is small (or no valid index exists). It deliberately does NOT cast
-- vec to a fixed dimension, so it never matches the HNSW expression index and always scans exactly —
-- correct for a small set, and correct in the window before the index is built. Same filter shape as the
-- count. (The index-backed path is a dynamically-built query because the vector(D) typmod cannot be a bind
-- parameter — see core/retrieval.)
SELECT m.id, m.content, m.kind, (e.vec <=> sqlc.arg(query_vec)::vector)::float8 AS distance
FROM memories m
JOIN embeddings e ON e.project_id = m.project_id AND e.memory_id = m.id
WHERE m.project_id = sqlc.arg(project_id) AND e.project_id = sqlc.arg(project_id)
  AND e.model_id = sqlc.arg(model_id)
  AND m.superseded_by IS NULL AND m.valid_to IS NULL
  AND (cardinality(sqlc.arg(scopes)::text[]) = 0 OR m.scope_keys && sqlc.arg(scopes)::text[])
  AND (sqlc.arg(include_quarantine)::bool OR m.trust_tier <> 'quarantine')
ORDER BY distance ASC
LIMIT sqlc.arg(max_results)::int;
