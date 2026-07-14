-- +goose Up

-- context_hash is the entity-bucket key for near-duplicate dedup: a hash the write path computes over a
-- memory's kind and entity context — the content-less prefix of content_hash (same encoder and version
-- constant, so the two rotate together). content_hash finds an EXACT restatement; context_hash finds the
-- BUCKET of live memories in the same entity context, which the consolidation path then compares by
-- embedding similarity (a near-duplicate merges into the most-similar one above a threshold). Keeping the
-- similarity search inside this bucket is the blocking step that avoids an O(N) whole-project scan and the
-- false merges a tenant-wide comparison would invite. NULL for a memory written without a fingerprint
-- (e.g. a manual write that opts out of dedup), which the partial index below excludes. Existing rows
-- carry NULL: their entity context was never persisted, so — like content_hash in 0013 — there is nothing
-- to backfill; only rows written from here on are bucketed.
ALTER TABLE memories ADD COLUMN context_hash bytea;

-- Blocking index for the similarity probe "which live memories share this entity context?". Partial over
-- only live rows — a superseded or expired memory is out of the dedup set — matching the same predicate as
-- memories_content_hash_idx, so the probe is one index range-scan and history rows never match.
-- Deliberately NOT UNIQUE: many live memories legitimately share an entity context (distinct facts about
-- the same entities); the index groups the bucket, it does not constrain it.
CREATE INDEX memories_context_hash_idx ON memories (project_id, context_hash)
    WHERE context_hash IS NOT NULL AND superseded_by IS NULL AND valid_to IS NULL;

-- +goose Down

-- Reverse of Up. Dropping the column drops its partial index too, but drop it explicitly first so the
-- intent is legible and the down migration does not rely on cascade order.
DROP INDEX IF EXISTS memories_context_hash_idx;
ALTER TABLE memories DROP COLUMN context_hash;
