-- name: PackRawTailGuaranteed :many
-- The read-your-writes window: a run's not-yet-distilled events from just past the extraction checkpoint up
-- to and including the seq the caller asked to see (min_seq). These are ALWAYS returned in full — the pack's
-- correctness guarantee that a reader sees a write it knows happened — so this query carries no cap (the
-- window is bounded by min_seq itself). Empty when min_seq is at or below the checkpoint. covered_seq is
-- passed in, read once by the caller, so every part of one pack sees the same checkpoint. Project-scoped, and
-- the UNIQUE (run_id, seq) index backs the range scan.
SELECT id, agent_id, payload, created_at, seq
FROM events
WHERE project_id = sqlc.arg(project_id) AND run_id = sqlc.arg(run_id)
  AND seq > sqlc.arg(covered_seq) AND seq <= sqlc.arg(min_seq)
ORDER BY seq;

-- name: PackRawTailBeyond :many
-- The recent-context tail: a run's not-yet-distilled events PAST the guaranteed read-your-writes window (seq
-- greater than BOTH the checkpoint and min_seq), newest first, capped. This is best-effort extra context — a
-- stalled extraction can leave many uncovered events, and this bounds how many the pack carries beyond what
-- the caller explicitly asked to see, so a pack can never grow unboundedly. The caller fetches one more than
-- the cap to detect truncation, then reverses into seq order. When min_seq is at or below the checkpoint the
-- lower bound collapses to the checkpoint, so this returns the newest uncovered events. Project-scoped.
SELECT id, agent_id, payload, created_at, seq
FROM events
WHERE project_id = sqlc.arg(project_id) AND run_id = sqlc.arg(run_id)
  AND seq > GREATEST(sqlc.arg(covered_seq)::bigint, sqlc.arg(min_seq)::bigint)
ORDER BY seq DESC
LIMIT sqlc.arg(max_events)::int;

-- name: PackFreshness :one
-- How far behind the run's distilled view is: the age, in milliseconds by the database clock, of the OLDEST
-- event past the extraction checkpoint — the longest a not-yet-distilled write has waited. Zero when the run
-- is fully caught up (no uncovered events). This is the single definition of the pack's freshness lag.
-- covered_seq is passed in (read once) so it agrees with the raw tail. Project-scoped.
SELECT coalesce(extract(epoch FROM now() - min(events.created_at)) * 1000, 0)::bigint AS freshness_lag_ms
FROM events
WHERE events.project_id = sqlc.arg(project_id) AND events.run_id = sqlc.arg(run_id)
  AND events.seq > sqlc.arg(covered_seq);

-- name: InsertPackLog :exec
-- One row per context-pack request, for run trace and observability. It is written in the SAME transaction as
-- the pack's reads, so the pack and its trace commit together — a failed insert fails the pack, and there is
-- never an unaccounted pack. tokens_saved is passed NULL in this increment (a downstream metering pass defines
-- it from est_source_tokens/packed_tokens); pack_hash is NULL until a later increment computes a byte-stable
-- digest. project_id is explicit for tenant isolation; run_id is the run the pack was taken in.
INSERT INTO pack_logs (
    project_id, run_id, query,
    covered_seq, freshness_lag_ms, latency_ms,
    scopes, token_budget, est_source_tokens, packed_tokens, tokens_saved,
    memory_ids, pack_hash
) VALUES (
    sqlc.arg(project_id), sqlc.arg(run_id), sqlc.arg(query),
    sqlc.arg(covered_seq), sqlc.arg(freshness_lag_ms), sqlc.arg(latency_ms),
    sqlc.arg(scopes), sqlc.arg(token_budget), sqlc.arg(est_source_tokens), sqlc.arg(packed_tokens), sqlc.arg(tokens_saved),
    sqlc.arg(memory_ids), sqlc.arg(pack_hash)
);
