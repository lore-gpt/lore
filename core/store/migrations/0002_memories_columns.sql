-- +goose Up

-- Phase 1 completes the `memories` table (Phase 0 shipped a placeholder). The write
-- and consolidation paths need bi-temporal validity, provenance, versioning, and
-- governance columns. The advanced-behavior columns (`trust_tier`, `review_status`)
-- ship in the single shared schema from day one and default to basic behavior here —
-- they are read and written, just never varied (no forked tables).
--
--   entities         denormalized entity mentions extracted alongside the memory
--   valid_from/to    bi-temporal validity window (valid_to IS NULL => currently valid)
--   superseded_by    points at the memory that replaced this one (version chain head
--                    is the row where superseded_by IS NULL)
--   trust_tier       provenance trust tier; defaults to 'normal'
--   review_status    governance gate; defaults to 'auto_approved'
--   created_by_agent originating agent id; NULL for manual (non-agent) writes
--   source_event_id  the raw event this memory was extracted from; NULL for manual writes
ALTER TABLE memories
    ADD COLUMN entities         jsonb       NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN valid_from       timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN valid_to         timestamptz,
    ADD COLUMN superseded_by    uuid        REFERENCES memories (id) ON DELETE SET NULL,
    ADD COLUMN trust_tier       text        NOT NULL DEFAULT 'normal',
    ADD COLUMN review_status    text        NOT NULL DEFAULT 'auto_approved',
    ADD COLUMN created_by_agent text,
    ADD COLUMN source_event_id  uuid        REFERENCES events (id) ON DELETE SET NULL;

-- `kind` is a closed vocabulary (kept as text + CHECK, matching how trust_tier and
-- review_status model their value sets, rather than a native enum which is awkward to
-- evolve). Named so the down migration can drop it — the column itself predates 0002.
ALTER TABLE memories
    ADD CONSTRAINT memories_kind_check
    CHECK (kind IN ('working', 'episodic', 'semantic', 'procedural'));

-- Drop the Phase 0 inline embedding column. Embeddings relocate to a dedicated,
-- dimension-free `embeddings` table in a later migration, so retrieval always queries
-- exactly one model's vector space per project (the embedding model is not yet fixed).
ALTER TABLE memories DROP COLUMN embedding;

-- +goose Down

-- Reverse of Up. Restore the inline embedding column first, then drop the CHECK
-- (its column `kind` survives), then drop the columns 0002 added — dropping
-- superseded_by / source_event_id also drops their inline foreign keys.
ALTER TABLE memories ADD COLUMN embedding vector;

ALTER TABLE memories DROP CONSTRAINT memories_kind_check;

ALTER TABLE memories
    DROP COLUMN source_event_id,
    DROP COLUMN created_by_agent,
    DROP COLUMN review_status,
    DROP COLUMN trust_tier,
    DROP COLUMN superseded_by,
    DROP COLUMN valid_to,
    DROP COLUMN valid_from,
    DROP COLUMN entities;
