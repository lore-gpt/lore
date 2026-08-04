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
-- the one printed when the key was minted). A already-revoked key updates no row, so zero rows means "either
-- unknown or already revoked" — the two are NOT distinguishable from this result alone. The caller tells them
-- apart with GetAPIKeyRevokedAt below, which only runs once this has already reported no change.
UPDATE api_keys
SET revoked_at = now()
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: GetAPIKeyRevokedAt :one
-- lore:tenant-exempt: the follow-up read for the CLI's revoke, in the same admin context as RevokeAPIKey and
-- keyed by the same operator-held id. It exists to turn "no rows affected" into an answer: no row here means
-- the id is unknown, a row means it was revoked already and when. Deliberately NOT merged into the UPDATE as
-- a CTE — a CTE's SELECT arm reads the statement-start snapshot while the UPDATE sees the post-lock row, and
-- that divergence has already produced a wrong answer elsewhere in this schema. Two statements on a failed
-- path cost one extra round trip and cannot disagree with themselves.
SELECT revoked_at
FROM api_keys
WHERE id = sqlc.arg(id);

-- name: ListProjectAPIKeys :many
-- Every key minted for one project, newest first, WITHOUT the hash — the raw token is unrecoverable by design
-- and the stored hash is not something an operator ever needs to see. key_prefix is what makes a row
-- recognisable, and this is the query it was added for. Revoked keys are included on purpose: the revocation
-- history is what an operator is usually looking for. project_id is in the predicate, so this is tenant-scoped
-- by construction.
SELECT id, name, key_prefix, created_at, revoked_at
FROM api_keys
WHERE project_id = sqlc.arg(project_id)
ORDER BY created_at DESC, id;
