-- name: UpsertEntity :one
-- Get-or-create the entity named (project_id, name), returning its id. On conflict the no-op SET
-- (name = itself) is what lets RETURNING yield the existing row's id — ON CONFLICT DO NOTHING would
-- return no row. type and aliases are kept as first written (a project-scoped type registry and
-- alias-merge resolution land in a later phase); this is how the write path resolves a mention or a
-- claim's subject to an entity id.
INSERT INTO entities (project_id, name, type, aliases)
VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id, name) DO UPDATE SET name = EXCLUDED.name
RETURNING id;
