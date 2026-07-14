-- name: LookupAPIKeyProject :one
-- lore:tenant-exempt: auth bootstrap — resolves the project FROM the key, before any tenant scope is known, so
-- it must run unscoped (the caller then sets lore.project_id from the result). A revoked key (revoked_at set)
-- returns no row, so a revoked or unknown key are indistinguishable to the caller — no cross-tenant existence
-- oracle. Backed by the UNIQUE (key_hash) index (a single indexed probe). NOTE (lore_app cutover): today the
-- application role bypasses RLS so this bare read works; once the app runs as a subject role, this exact query
-- must move behind a SECURITY DEFINER function owned by the migration role, or RLS will scope it to a project
-- that is not yet known and it will return nothing. See the cutover backlog.
SELECT project_id
FROM api_keys
WHERE key_hash = sqlc.arg(key_hash) AND revoked_at IS NULL;

-- name: CreateAPIKey :one
-- Mint one API key for a project: the caller has already hashed the raw token (never stored) and taken its
-- non-secret prefix. project_id is in the column list, so this is tenant-scoped by construction.
INSERT INTO api_keys (project_id, name, key_prefix, key_hash)
VALUES (sqlc.arg(project_id), sqlc.arg(name), sqlc.arg(key_prefix), sqlc.arg(key_hash))
RETURNING id, project_id, name, key_prefix, created_at;

-- name: RevokeAPIKey :execrows
-- lore:tenant-exempt: operator revokes a key by its id (an admin CLI action with no tenant context; the id is
-- the one printed when the key was minted). Idempotent-ish: a already-revoked key updates no row, so the
-- caller can report "not found or already revoked" from a zero row count.
UPDATE api_keys
SET revoked_at = now()
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;
