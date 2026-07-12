-- +goose Up

-- Per-run extraction checkpoint: the highest event seq an extraction pass has already consumed for
-- this run. The coalesced extract_run job reads only events past it (seq > covered_seq) and advances
-- it — atomically with the memory/claim writes of that pass — once the pass commits. That single
-- transaction is what makes extraction idempotent: a committed pass moves the checkpoint so its
-- events are never reprocessed, while a pass that crashes mid-write rolls back the advance too and
-- is retried cleanly. It also turns the debounce's event-count window into "events since the
-- checkpoint" and lets a pass detect a tail that arrived while it ran (seq still beyond the advanced
-- checkpoint) and drain it. Starts at 0 (no events consumed); seq is 1-based, so 0 covers nothing.
ALTER TABLE runs ADD COLUMN covered_seq bigint NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE runs DROP COLUMN covered_seq;
