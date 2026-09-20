-- name: CreateRun :one
-- Create a run in the caller's project and return its id and start time. project_id is supplied by the
-- authenticated key (never a client body), and it is carried on the inserted row, so a run can only be
-- born in the key's own project — tenant-scoped by construction. The handler runs this inside a
-- project-scoped transaction (WithProject); the request carries no other run fields today.
INSERT INTO runs (project_id)
VALUES (sqlc.arg(project_id))
RETURNING id, started_at;

-- name: GetRunProject :one
-- lore:tenant-exempt: operator maintenance lookup, by run id, over the CLI's own database connection.
-- There is no request and no authenticated key here to scope to — the run id IS the operator's input, and
-- ids are unique across projects, so this resolves which tenant a run belongs to rather than assuming one.
-- Nothing on a request path may use it: a handler already knows its project and must filter by it.
SELECT project_id FROM runs WHERE id = sqlc.arg(run_id);
