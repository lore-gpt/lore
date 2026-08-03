-- name: UpsertWorkingFact :execrows
-- Record a run's current value for one subject, from the transaction that appends the state event
-- carrying it. Sharing that transaction is the whole durability story: the event and the fact behind
-- it commit together or not at all, so a caller holding an event's seq is guaranteed a pack can
-- already see the fact.
--
-- The seq guard keeps the row's meaning local and self-evident — `seq` is always the highest event
-- seq that has written this subject — rather than something a reader has to infer from elsewhere.
-- Concurrent writers of one run are already serialised upstream (InsertEvent bumps the run counter
-- with a row lock held to commit), so the guard is not what orders today's ingest; it is what keeps
-- the invariant true for anything that is NOT serialised that way — a replay, a backfill, or a
-- second writer added later — and it makes a redelivered write a no-op instead of a rewrite.
--
-- :execrows, deliberately not :one. When the guard suppresses a stale write the statement affects no
-- rows; with RETURNING that would surface as ErrNoRows and a caller would have to treat "a newer
-- value already won" as a failure — rolling back a perfectly good event. Zero rows here is a normal
-- outcome, not an error.
INSERT INTO working_facts (project_id, run_id, entity, predicate, value, seq, agent_id)
VALUES (sqlc.arg(project_id), sqlc.arg(run_id), sqlc.arg(entity), sqlc.arg(predicate),
        sqlc.arg(value), sqlc.arg(seq), sqlc.arg(agent_id))
ON CONFLICT (project_id, run_id, entity, predicate) DO UPDATE
SET value      = EXCLUDED.value,
    seq        = EXCLUDED.seq,
    agent_id   = EXCLUDED.agent_id,
    updated_at = now()
WHERE working_facts.seq < EXCLUDED.seq;

-- name: ListRunWorkingFacts :many
-- One run's working memory, freshest first — the pack's working section. Scoped to the run, not the
-- project: two runs may hold different values for the same subject, and each must see its own.
-- Served by the primary key as a prefix range scan.
--
-- The ORDER BY gives the query a stable plan and a declared contract, but the pack re-sorts in Go:
-- Postgres orders text by the database's collation while the pack's determinism guarantee is over
-- bytes, so leaving the final order to SQL would make a pack's bytes depend on the server's locale.
SELECT entity, predicate, value, seq, agent_id
FROM working_facts
WHERE project_id = sqlc.arg(project_id) AND run_id = sqlc.arg(run_id)
ORDER BY seq DESC, entity, predicate;
