-- name: InsertEvent :one
-- Assigns the event's per-run seq inside the write transaction. A data-modifying CTE bumps the
-- run's counter — a single-row UPDATE ... RETURNING whose row lock serialises concurrent writers
-- of the same run, so seq is gap-free and monotonic without an advisory lock — and the INSERT
-- stamps the returned value onto the event. project_id is taken from the same run row, so it can
-- never disagree with the run's project (the composite (project_id, run_id) foreign key backstops
-- it). An unknown run_id updates no row, the CTE is empty, nothing is inserted, and no row is
-- returned (pgx.ErrNoRows) — the caller's "unknown run" signal.
WITH bumped AS (
    UPDATE runs AS r
    SET last_seq = r.last_seq + 1
    WHERE r.id = sqlc.arg(run_id)
    RETURNING r.id, r.project_id, r.last_seq
)
INSERT INTO events (project_id, run_id, agent_id, payload, seq)
SELECT bumped.project_id, bumped.id, sqlc.arg(agent_id), sqlc.arg(payload), bumped.last_seq
FROM bumped
RETURNING events.id, events.run_id, events.agent_id, events.payload, events.created_at, events.seq, events.project_id;

-- name: GetEvent :one
SELECT id, run_id, agent_id, payload, created_at, seq, project_id
FROM events
WHERE project_id = $1 AND id = $2;

-- name: ListRunEvents :many
-- A run's events in seq order, for the coalesced extraction pass. Project-scoped so it is tenant-
-- safe and (under RLS) reads only the caller's project.
SELECT id, run_id, agent_id, payload, created_at, seq, project_id
FROM events
WHERE project_id = $1 AND run_id = $2
ORDER BY seq;

-- name: CountAllEvents :one
-- lore:tenant-exempt: global by design; must run under a bypass role (readonly/ops), NOT lore_app — RLS would silently scope it
SELECT count(*) FROM events;
