-- name: InsertMemoryScope :one
INSERT INTO memory_scopes (project_id, memory_id, scope_type, scope_id)
VALUES ($1, $2, $3, $4)
RETURNING memory_id, scope_type, scope_id, created_at, project_id;

-- name: ListMemoryScopes :many
SELECT memory_id, scope_type, scope_id, created_at, project_id
FROM memory_scopes
WHERE project_id = $1 AND memory_id = $2
ORDER BY scope_type, scope_id;
