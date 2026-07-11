-- name: UpsertEmbedding :one
INSERT INTO embeddings (project_id, memory_id, model_id, vec)
VALUES ($1, $2, $3, $4)
ON CONFLICT (memory_id, model_id) DO UPDATE SET vec = EXCLUDED.vec
RETURNING project_id, memory_id, model_id, vec, created_at;

-- name: GetEmbedding :one
SELECT project_id, memory_id, model_id, vec, created_at
FROM embeddings
WHERE memory_id = $1 AND model_id = $2;
