-- name: InsertMemoryScope :one
INSERT INTO memory_scopes (memory_id, scope_type, scope_id)
VALUES ($1, $2, $3)
RETURNING memory_id, scope_type, scope_id, created_at;

-- name: ListMemoryScopes :many
SELECT memory_id, scope_type, scope_id, created_at
FROM memory_scopes
WHERE memory_id = $1
ORDER BY scope_type, scope_id;
