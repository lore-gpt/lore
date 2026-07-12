-- name: InsertEvent :one
-- project_id is derived from the run so it can never disagree with the run's project (and the
-- composite (project_id, run_id) foreign key backstops it). An unknown run_id matches no row, so
-- nothing is inserted and no row is returned (pgx.ErrNoRows) — the caller's "unknown run" signal.
INSERT INTO events (project_id, run_id, agent_id, payload)
SELECT r.project_id, r.id, sqlc.arg(agent_id), sqlc.arg(payload)
FROM runs r
WHERE r.id = sqlc.arg(run_id)
RETURNING id, run_id, agent_id, payload, created_at, seq, project_id;

-- name: GetEvent :one
SELECT id, run_id, agent_id, payload, created_at, seq, project_id
FROM events
WHERE id = $1;

-- name: CountEvents :one
SELECT count(*) FROM events;
