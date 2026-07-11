-- Bootstrap inserts for the org -> project -> run chain an event needs, plus the
-- API key row. Used by wiring and tests until a control plane owns these.

-- name: InsertOrganization :one
INSERT INTO organizations (name)
VALUES ($1)
RETURNING id, name, created_at;

-- name: InsertProject :one
INSERT INTO projects (org_id, name)
VALUES ($1, $2)
RETURNING id, org_id, name, created_at, active_model_id;

-- name: InsertAPIKey :one
INSERT INTO api_keys (project_id, key_hash)
VALUES ($1, $2)
RETURNING id, project_id, key_hash, created_at, revoked_at;

-- name: InsertRun :one
INSERT INTO runs (project_id)
VALUES ($1)
RETURNING id, project_id, status, started_at;
