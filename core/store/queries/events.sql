-- name: InsertEvent :one
INSERT INTO events (run_id, agent_id, payload)
VALUES ($1, $2, $3)
RETURNING id, run_id, agent_id, payload, created_at, seq;

-- name: GetEvent :one
SELECT id, run_id, agent_id, payload, created_at, seq
FROM events
WHERE id = $1;

-- name: CountEvents :one
SELECT count(*) FROM events;
