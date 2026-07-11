-- +goose Up

-- Phase 1 adds the three tables the write and consolidation paths build on:
-- per-memory version history, structured claims, and scope tags. They ship in the
-- shared schema now; the logic that fills them (extraction, consolidation, scope
-- filtering) lands in later increments. OSS reads and writes them from day one.

-- memory_versions is the full edit history of a memory. Each row is the content of
-- one version; the live memory row carries the current version number. A memory has
-- at most one row per version number, so (memory_id, version) is the natural key.
--   changed_by  originating agent/actor for this version; NULL for manual edits
--   reason      why this version was written (e.g. the consolidation decision that
--               produced it); NULL for the initial version
CREATE TABLE memory_versions (
    memory_id  uuid        NOT NULL REFERENCES memories (id) ON DELETE CASCADE,
    version    integer     NOT NULL,
    content    text        NOT NULL,
    changed_by text,
    reason     text,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (memory_id, version)
);

-- claims are structured (entity, predicate, value) assertions extracted from a
-- memory. superseded_by chains a claim to the one that replaced it — the chain head
-- is the row where superseded_by IS NULL. The write path is responsible for pointing
-- a superseded claim at a same-subject replacement; the schema enforces the invariant
-- that matters for conflict detection (one active claim per subject, below) and
-- forbids a claim from superseding itself (claims_no_self_supersede).
--   entity_id   the subject the claim is about. No foreign key yet: the entity
--               registry table arrives in a later migration, which is when a
--               reference can be added.
--   value       the asserted value as JSON, so structured and scalar claims share
--               one column.
--   event_time  when the claim holds in the world; set only for time-bound claims.
--               Write ordering always comes from the run sequence, never this field.
--
-- superseded_by is DEFERRABLE INITIALLY DEFERRED so a subject can be re-stated in a
-- single transaction: mark the current claim superseded, then insert its replacement
-- with the id it now points at. The self-reference only has to hold at commit, by
-- which time the replacement row exists. Without deferral the partial-unique index
-- below (one active claim per subject) makes that swap impossible — the replacement
-- can't be inserted while the old row is still active, and the old row can't be
-- superseded before the replacement it must point at exists.
--
-- ON DELETE is NO ACTION (unlike memories.superseded_by, which is SET NULL): SET NULL
-- would silently reactivate a superseded claim if the row it points at were deleted,
-- and for a chain of three or more that resurrects a predecessor alongside the still
-- active head — two active claims for one subject, breaking the invariant. NO ACTION
-- (deferred) instead refuses to delete a claim another still points at, while a
-- whole-tenant delete that removes the entire chain in one transaction still commits.
CREATE TABLE claims (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    memory_id     uuid        NOT NULL REFERENCES memories (id) ON DELETE CASCADE,
    project_id    uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    entity_id     uuid        NOT NULL,
    predicate     text        NOT NULL,
    value         jsonb       NOT NULL,
    event_time    timestamptz,
    superseded_by uuid        REFERENCES claims (id) ON DELETE NO ACTION
                              DEFERRABLE INITIALLY DEFERRED,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT claims_no_self_supersede CHECK (superseded_by IS NULL OR superseded_by <> id)
);

-- At most one active (non-superseded) claim per (project, entity, predicate). This
-- is the substrate for conflict detection: a second active assertion about the same
-- subject and predicate is rejected until the first is superseded. Superseded rows
-- are excluded, so historical claims accumulate without bound.
CREATE UNIQUE INDEX claims_active_subject_key
    ON claims (project_id, entity_id, predicate)
    WHERE superseded_by IS NULL;

-- Cover the cascade paths and per-memory lookups. The unique index above already
-- serves active-row lookups keyed on (project_id, entity_id, predicate); these two
-- serve full-history scans by memory and the delete-cascade from parent rows.
CREATE INDEX claims_memory_id_idx ON claims (memory_id);
CREATE INDEX claims_project_id_idx ON claims (project_id);

-- memory_scopes tags a memory with the run / agent / team / org contexts it belongs
-- to. scope_id is text so it can hold any of those identifiers uniformly. A memory
-- carries each (scope_type, scope_id) tag at most once.
CREATE TABLE memory_scopes (
    memory_id  uuid        NOT NULL REFERENCES memories (id) ON DELETE CASCADE,
    scope_type text        NOT NULL,
    scope_id   text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (memory_id, scope_type, scope_id),
    CONSTRAINT memory_scopes_scope_type_check
        CHECK (scope_type IN ('run', 'agent', 'team', 'org'))
);

-- Reverse lookup: which memories carry a given scope tag.
CREATE INDEX memory_scopes_scope_idx ON memory_scopes (scope_type, scope_id);

-- Denormalized, indexable copy of a memory's scope tags as `scope_type:scope_id`
-- keys, so scope filtering during retrieval is a single indexed array-containment
-- check instead of a join. The write path populates it in a later increment; the
-- column and its GIN index exist now so that step is a data change, not a migration.
ALTER TABLE memories
    ADD COLUMN scope_keys text[] NOT NULL DEFAULT '{}'::text[];

CREATE INDEX memories_scope_keys_gin ON memories USING gin (scope_keys);

-- +goose Down

-- Reverse of Up, dropping dependents before parents. Dropping the column removes its
-- GIN index; dropping the tables removes their indexes and constraints.
DROP INDEX IF EXISTS memories_scope_keys_gin;
ALTER TABLE memories DROP COLUMN IF EXISTS scope_keys;
DROP TABLE IF EXISTS memory_scopes;
DROP TABLE IF EXISTS claims;
DROP TABLE IF EXISTS memory_versions;
