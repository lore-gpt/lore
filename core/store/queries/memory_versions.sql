-- name: InsertMemoryVersion :one
INSERT INTO memory_versions (memory_id, version, content, changed_by, reason)
VALUES ($1, $2, $3, $4, $5)
RETURNING memory_id, version, content, changed_by, reason, created_at;

-- name: ListMemoryVersions :many
SELECT memory_id, version, content, changed_by, reason, created_at
FROM memory_versions
WHERE memory_id = $1
ORDER BY version;
