-- name: CreateRun :one
-- Create a run in the caller's project and return its id and start time. project_id is supplied by the
-- authenticated key (never a client body), and it is carried on the inserted row, so a run can only be
-- born in the key's own project — tenant-scoped by construction. The handler runs this inside a
-- project-scoped transaction (WithProject); the request carries no other run fields today.
INSERT INTO runs (project_id)
VALUES (sqlc.arg(project_id))
RETURNING id, started_at;
