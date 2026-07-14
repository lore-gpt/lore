-- Bootstrap inserts for the org -> project -> run chain an event needs. Used by
-- wiring and tests until a control plane owns these. (API keys are minted through
-- CreateAPIKey in auth.sql / the `lore keys` command.)

-- name: InsertOrganization :one
INSERT INTO organizations (name)
VALUES ($1)
RETURNING id, name, created_at;

-- name: InsertProject :one
-- lore:tenant-exempt: creates the tenant (projects) row itself — there is no project to scope by.
-- The RETURNING list is a fixed subset (not the whole row): later migrations add columns to projects,
-- and listing only long-standing columns keeps this insert runnable at any schema version a migration
-- test down-migrates to. That is why it returns a bespoke InsertProjectRow, not the db.Project model.
INSERT INTO projects (org_id, name)
VALUES ($1, $2)
RETURNING id, org_id, name, created_at, active_model_id, retain_events_days, retain_memories_days;

-- name: InsertRun :one
INSERT INTO runs (project_id)
VALUES ($1)
RETURNING id, project_id, status, started_at, last_seq;
