-- name: InsertEvent :one
-- Assigns the event's per-run seq inside the write transaction. A data-modifying CTE bumps the
-- run's counter — a single-row UPDATE ... RETURNING whose row lock serialises concurrent writers
-- of the same run, so seq is gap-free and monotonic without an advisory lock — and the INSERT
-- stamps the returned value onto the event. project_id is taken from the same run row, so it can
-- never disagree with the run's project (the composite (project_id, run_id) foreign key backstops
-- it). An unknown run_id updates no row, the CTE is empty, nothing is inserted, and no row is
-- returned (pgx.ErrNoRows) — the caller's "unknown run" signal.
WITH bumped AS (
    UPDATE runs AS r
    SET last_seq = r.last_seq + 1
    WHERE r.id = sqlc.arg(run_id)
    RETURNING r.id, r.project_id, r.last_seq
)
INSERT INTO events (project_id, run_id, agent_id, payload, seq)
SELECT bumped.project_id, bumped.id, sqlc.arg(agent_id), sqlc.arg(payload), bumped.last_seq
FROM bumped
RETURNING events.id, events.run_id, events.agent_id, events.payload, events.created_at, events.seq, events.project_id;

-- name: GetEvent :one
SELECT id, run_id, agent_id, payload, created_at, seq, project_id
FROM events
WHERE project_id = $1 AND id = $2;

-- name: ListRunEvents :many
-- A run's not-yet-extracted events (seq past the run's checkpoint) in seq order, for the coalesced
-- extraction pass. The covered_seq subquery scopes the read to events an earlier pass has not
-- already consumed, so a re-enqueued or retried job never re-reads them. Project-scoped so it is
-- tenant-safe and (under RLS) reads only the caller's project.
SELECT id, run_id, agent_id, payload, created_at, seq, project_id
FROM events
WHERE events.project_id = $1 AND events.run_id = $2
  AND events.seq > (SELECT covered_seq FROM runs WHERE runs.id = $2)
ORDER BY events.seq;

-- name: RunExtractionReadiness :one
-- Debounce inputs for one run's coalesced extraction, measured over its not-yet-extracted events
-- (seq past the checkpoint): how many there are, and how long since the most recent one measured by
-- the database clock (so there is no app/DB clock skew). With none pending, event_count is 0 and
-- idle_seconds is 0, so the caller treats the drained run as ready (nothing to do). Project-scoped.
SELECT count(*)::bigint AS event_count,
       coalesce(extract(epoch FROM now() - max(events.created_at)), 0)::double precision AS idle_seconds
FROM events
WHERE events.project_id = $1 AND events.run_id = $2
  AND events.seq > (SELECT covered_seq FROM runs WHERE runs.id = $2);

-- name: AdvanceCoveredSeq :execrows
-- Advance a run's extraction checkpoint to the highest seq the pass consumed and clear any pending
-- batch state, in one single-row UPDATE, guarded by a compare-and-swap on the checkpoint's expected
-- value: the pass reads covered_seq before it does its work and passes that value back as
-- expected_covered_seq, so the advance commits only if no other pass moved the checkpoint in between.
-- A mismatch updates 0 rows; the caller treats that as "another pass already advanced this run" and
-- rolls back its own writes, so a concurrent double-delivery of the same window produces one set of
-- memories and one advance, never two. (The compare-and-swap also subsumes the old monotonic guard:
-- new_covered_seq is always past the expected value, so a match only ever moves the checkpoint
-- forward.) The batch columns are already NULL for a realtime pass — clearing them is a no-op there —
-- and drop the recorded handle for a collected economy pass. This runs in the same transaction as the
-- pass's memory writes, so the checkpoint, the rows it accounts for, and the batch clear commit
-- together — the atomicity that makes a coalesced pass idempotent.
UPDATE runs
SET covered_seq = sqlc.arg(new_covered_seq),
    extraction_batch_id = NULL,
    extraction_batch_covered_seq = NULL
WHERE id = sqlc.arg(run_id)
  AND project_id = sqlc.arg(project_id)
  AND covered_seq = sqlc.arg(expected_covered_seq);

-- name: GetRunExtractionState :one
-- The run's extraction checkpoint, its extraction mode (from its project), and any pending batch, in
-- one project-scoped read. covered_seq is the value the pass compare-and-swaps on when it advances the
-- checkpoint (see AdvanceCoveredSeq), read here so the whole decision starts from one consistent view.
-- The mode comes via a scalar subquery so the statement stays single-table (runs) for the generated
-- return type; a NULL extraction_batch_id means no batch is in flight and the pass is in its
-- submit/realtime phase, while a non-NULL one means an earlier attempt submitted a batch awaiting
-- collection. last_seq (the run's highest assigned seq) is read in the same row so a reader can validate a
-- requested min_seq against it without a second query.
SELECT r.covered_seq,
       r.last_seq,
       r.extraction_batch_id,
       r.extraction_batch_covered_seq,
       (SELECT extraction_mode FROM projects WHERE projects.id = r.project_id) AS extraction_mode
FROM runs r
WHERE r.id = sqlc.arg(run_id) AND r.project_id = sqlc.arg(project_id);

-- name: SetRunBatch :execrows
-- Record the handle and covered seq of the batch a run's economy-mode pass just submitted, so a later
-- attempt collects it. Project-scoped. AdvanceCoveredSeq clears these when the collected pass commits.
UPDATE runs
SET extraction_batch_id = sqlc.arg(batch_id),
    extraction_batch_covered_seq = sqlc.arg(batch_covered_seq)
WHERE id = sqlc.arg(run_id) AND project_id = sqlc.arg(project_id);

-- name: CountAllEvents :one
-- lore:tenant-exempt: global by design; must run under a bypass role (readonly/ops), NOT lore_app — RLS would silently scope it
SELECT count(*) FROM events;
