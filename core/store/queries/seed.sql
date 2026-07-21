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

-- name: ProjectExists :one
-- Does this project still exist? The provision command verifies the project a credentials file points to is
-- actually present before treating the file as proof of provisioning, so a wiped database (for example after
-- `docker compose down -v`, which drops the volume while the host credentials file lingers) is caught loudly
-- instead of serving a dead key. It runs at bootstrap under the owner role, before any tenant GUC is set, so
-- it must not be RLS-scoped.
-- lore:tenant-exempt: projects is the tenant root; scoped by its own id (the RLS subject), not project_id
SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1) AS project_exists;
