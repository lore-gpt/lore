-- +goose Up

-- entities: one node per (project, name). Promote that identity to a real uniqueness so the write
-- path can upsert an entity by name (get-or-create), replacing the plain lookup index (which the
-- unique index subsumes, including the project_id prefix the delete cascade uses). The table has no
-- writer before this increment, so it is empty and the constraint adds cleanly.
DROP INDEX entities_project_name_idx;
ALTER TABLE entities ADD CONSTRAINT entities_project_name_key UNIQUE (project_id, name);

-- claims.memory_id becomes nullable. A claim is a first-class structured assertion produced ALONGSIDE
-- the prose memory, not strictly a child of one: an event can yield a claim with no co-produced
-- memory. memory_id is set to the same-event memory when one exists — so deleting that memory still
-- cascades its claims (the composite FK's ON DELETE CASCADE) — and NULL for a standalone claim.
ALTER TABLE claims ALTER COLUMN memory_id DROP NOT NULL;

-- claims.source_event_id: the raw event a claim was distilled from, mirroring memories.source_event_id
-- (0002). Provenance never depends on memory_id — every extracted claim carries its originating event
-- here, so a standalone (memory_id NULL) claim is still traceable, and the graph's edge provenance and
-- a later delete cascade have a hook. ON DELETE SET NULL clears the pointer, not the claim, if the
-- event is purged; NULL for manual (non-extracted) claims.
ALTER TABLE claims ADD COLUMN source_event_id uuid REFERENCES events (id) ON DELETE SET NULL;

-- +goose Down

-- Reverse of Up. SET NOT NULL restores the original constraint; it assumes no standalone (memory_id
-- NULL) claims remain — true in production (empty until this increment; a reversal after writes must
-- clear them first, as the test does).
ALTER TABLE claims DROP COLUMN source_event_id;
ALTER TABLE claims ALTER COLUMN memory_id SET NOT NULL;
ALTER TABLE entities DROP CONSTRAINT entities_project_name_key;
CREATE INDEX entities_project_name_idx ON entities (project_id, name);
