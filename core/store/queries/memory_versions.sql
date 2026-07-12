-- name: InsertMemoryVersion :one
INSERT INTO memory_versions (project_id, memory_id, version, content, changed_by, reason)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING memory_id, version, content, changed_by, reason, created_at, project_id;

-- name: ListMemoryVersions :many
SELECT memory_id, version, content, changed_by, reason, created_at, project_id
FROM memory_versions
WHERE project_id = $1 AND memory_id = $2
ORDER BY version;
