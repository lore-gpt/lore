-- name: PinActiveModelIfUnset :execrows
-- First-wins pin: set the project's active embedding model, but only if it has none yet. The rows affected is
-- 1 for the pass that pins it and 0 if it was already pinned — by a prior pass or, under a concurrent race, by
-- the one winner. A losing racer blocks on the winner's row lock, and once the winner commits its re-checked
-- `active_model_id IS NULL` predicate is false, so it updates no row; exactly one racing first pass gets 1.
-- (A snapshot-read sibling CTE could not tell the loser apart from the winner under READ COMMITTED — the
-- conditional UPDATE's own row count can.) A 0 result means the caller must read the effective model in a
-- fresh statement (which sees the winner's commit) to reject a mismatch before writing vectors in a second
-- model's space; a 1 result means this pass chose the model, so it can trigger a one-time index build for the
-- newly pinned partition.
-- lore:tenant-exempt: projects is the tenant root; it is scoped by its own id (the RLS subject), not project_id
UPDATE projects
SET active_model_id = sqlc.arg(model_id)
WHERE id = sqlc.arg(project_id) AND active_model_id IS NULL;

-- name: ListProjectsWithActiveModel :many
-- Every project that has pinned an embedding model. The worker's startup sweep uses it to find projects
-- whose vector index is missing (pinned but the build was never enqueued or was lost to a crash) and enqueue
-- it, so no project is left permanently on the slower exact-scan path.
-- lore:tenant-exempt: a cross-tenant reconciliation scan run by the worker under the RLS-bypass role
SELECT id FROM projects WHERE active_model_id IS NOT NULL;
