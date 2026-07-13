-- +goose Up

-- content_hash is the fingerprint the consolidation path deduplicates on: a hash the write path computes
-- over a memory's kind, its entity context, and its normalized content (see the write path's fingerprint
-- function). Folding the entity context in keeps dedup inside an entity bucket — identical text in a
-- different context is not merged. Exact-content dedup is deliberately narrow — it merges only genuinely
-- identical restatements, never near duplicates — because a false merge silently loses a distinct memory,
-- while a missed duplicate is harmless. A later increment widens the matcher to vector similarity once
-- embeddings are written; the column and its index stay, only the lookup changes. NULL for a memory a
-- path writes without a fingerprint (e.g. a manual write that opts out of dedup), which the partial index
-- below excludes.
ALTER TABLE memories ADD COLUMN content_hash bytea;

-- Probe index for the dedup lookup "is there already a live memory with this fingerprint in the
-- project?". Partial over only live rows — a superseded (superseded_by set) or expired (valid_to set)
-- memory is out of the dedup set — so the lookup is one index probe and history rows never match.
-- Deliberately NOT UNIQUE: a unique constraint would force a merge on every content collision, which is
-- exactly the false merge the design rejects; the lookup instead drives an explicit merge-or-insert
-- decision, and a lost race between two passes leaves a harmless duplicate rather than a wrong merge.
CREATE INDEX memories_content_hash_idx ON memories (project_id, content_hash)
    WHERE content_hash IS NOT NULL AND superseded_by IS NULL AND valid_to IS NULL;

-- +goose Down

-- Reverse of Up. Dropping the column drops its partial index too, but drop it explicitly first so the
-- intent is legible and the down migration does not rely on cascade order.
DROP INDEX IF EXISTS memories_content_hash_idx;
ALTER TABLE memories DROP COLUMN content_hash;
