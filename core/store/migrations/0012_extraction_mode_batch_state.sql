-- +goose Up

-- Per-project extraction mode: whether this project's coalesced extraction runs on the low-latency
-- "realtime" path (a synchronous provider call) or the cost-optimised "economy" path (submit the
-- window to a provider's batch interface and collect the result minutes later). Set per project; a
-- run reads its project's mode to decide how to extract. Defaults to 'realtime'.
ALTER TABLE projects ADD COLUMN extraction_mode text NOT NULL DEFAULT 'realtime'
    CHECK (extraction_mode IN ('realtime', 'economy'));

-- Pending batch state for a run's economy-mode pass. A coalesced pass in economy mode submits its
-- window to the provider, records the returned handle plus the seq the window spans, then snoozes; a
-- later attempt collects the result and advances the checkpoint, which clears these back to NULL.
-- NULL extraction_batch_id means no batch is in flight for the run. extraction_batch_covered_seq is
-- the checkpoint that the collected pass will advance to — the highest seq the submitted window read —
-- so events that arrive while the batch is processing are left for the next pass, exactly as the
-- realtime tail-drain leaves them. Nullable and unconstrained by a foreign key: the handle is an
-- opaque provider string, not a local row.
ALTER TABLE runs ADD COLUMN extraction_batch_id text;
ALTER TABLE runs ADD COLUMN extraction_batch_covered_seq bigint;

-- +goose Down

ALTER TABLE runs DROP COLUMN extraction_batch_covered_seq;
ALTER TABLE runs DROP COLUMN extraction_batch_id;
ALTER TABLE projects DROP COLUMN extraction_mode;
