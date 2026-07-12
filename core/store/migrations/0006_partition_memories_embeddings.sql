-- +goose Up

-- Physically isolate each project's rows: memories and embeddings become LIST-partitioned
-- on project_id, so the most selective filter (tenant) is a partition boundary rather than a
-- WHERE clause — the vector index per partition stays small and its recall stays honest, and
-- deleting a tenant is dropping a partition. Per-project partitions are created at runtime by
-- the partition helper; this migration ships only the partitioned parents (empty).
--
-- A partitioned parent must include its partition key in every unique constraint, so the
-- primary keys gain project_id: memories (project_id, id), embeddings (project_id, memory_id,
-- model_id). That in turn makes id alone no longer unique, so every foreign key that pointed at
-- memories(id) becomes composite (project_id, memory_id) -> memories(project_id, id) — which
-- forces project_id onto the child tables that lacked it (memory_versions, memory_scopes), the
-- same necessity that already put project_id on claims and embeddings.
--
-- All target tables are empty at this point (no seed rows), so the rebuild drops nothing real.

-- Drop the child-of-memories and memories itself; CASCADE removes the inbound FK constraints on
-- claims / memory_versions / memory_scopes (re-added below as composite).
DROP TABLE embeddings;
DROP TABLE memories CASCADE;

-- memories, partitioned. Columns are the pre-0006 set, verbatim and in order (so generated code
-- is unchanged). superseded_by's self-FK is now composite; ON DELETE SET NULL names only the
-- pointer column so it does not try to null the NOT NULL partition key. source_event_id -> events
-- is unaffected (events is not partitioned).
CREATE TABLE memories (
    id               uuid        NOT NULL DEFAULT gen_random_uuid(),
    project_id       uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    kind             text        NOT NULL,
    content          text        NOT NULL,
    version          integer     NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL DEFAULT now(),
    entities         jsonb       NOT NULL DEFAULT '[]'::jsonb,
    valid_from       timestamptz NOT NULL DEFAULT now(),
    valid_to         timestamptz,
    superseded_by    uuid,
    trust_tier       text        NOT NULL DEFAULT 'normal',
    review_status    text        NOT NULL DEFAULT 'auto_approved',
    created_by_agent text,
    source_event_id  uuid        REFERENCES events (id) ON DELETE SET NULL,
    scope_keys       text[]      NOT NULL DEFAULT '{}'::text[],
    PRIMARY KEY (project_id, id),
    CONSTRAINT memories_kind_check CHECK (kind IN ('working', 'episodic', 'semantic', 'procedural')),
    CONSTRAINT memories_superseded_fk
        FOREIGN KEY (project_id, superseded_by) REFERENCES memories (project_id, id)
        ON DELETE SET NULL (superseded_by)
) PARTITION BY LIST (project_id);

CREATE INDEX memories_scope_keys_gin ON memories USING gin (scope_keys);

-- embeddings, partitioned. vec stays dimensionless; per-partition HNSW is built at runtime once a
-- model dimension is known (a following increment), not here.
CREATE TABLE embeddings (
    project_id uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    memory_id  uuid        NOT NULL,
    model_id   text        NOT NULL,
    vec        vector      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, memory_id, model_id),
    CONSTRAINT embeddings_memory_fk
        FOREIGN KEY (project_id, memory_id) REFERENCES memories (project_id, id) ON DELETE CASCADE
) PARTITION BY LIST (project_id);

CREATE INDEX embeddings_project_id_idx ON embeddings (project_id);

-- memory_versions / memory_scopes gain project_id (empty -> NOT NULL is free), their PKs lead with
-- (project_id, memory_id) so the PK index also backs the composite FK cascade, and their inbound
-- FKs become composite. ON DELETE CASCADE: a deleted memory takes its history and scope tags.
ALTER TABLE memory_versions ADD COLUMN project_id uuid NOT NULL;
ALTER TABLE memory_versions DROP CONSTRAINT memory_versions_pkey;
ALTER TABLE memory_versions ADD PRIMARY KEY (project_id, memory_id, version);
ALTER TABLE memory_versions
    ADD CONSTRAINT memory_versions_memory_fk
    FOREIGN KEY (project_id, memory_id) REFERENCES memories (project_id, id) ON DELETE CASCADE;

ALTER TABLE memory_scopes ADD COLUMN project_id uuid NOT NULL;
ALTER TABLE memory_scopes DROP CONSTRAINT memory_scopes_pkey;
ALTER TABLE memory_scopes ADD PRIMARY KEY (project_id, memory_id, scope_type, scope_id);
ALTER TABLE memory_scopes
    ADD CONSTRAINT memory_scopes_memory_fk
    FOREIGN KEY (project_id, memory_id) REFERENCES memories (project_id, id) ON DELETE CASCADE;

-- claims already carries project_id and keeps its surrogate id PK, so it needs the composite FK
-- re-added plus a dedicated (project_id, memory_id) index to back the cascade.
ALTER TABLE claims
    ADD CONSTRAINT claims_memory_fk
    FOREIGN KEY (project_id, memory_id) REFERENCES memories (project_id, id) ON DELETE CASCADE;
CREATE INDEX claims_project_memory_idx ON claims (project_id, memory_id);

-- +goose Down

-- Reverse to the pre-0006 plain shape. First empty the partitioned parent while its ON DELETE
-- CASCADE composite child FKs are still attached: the cascade physically removes the referencing
-- rows in claims / memory_versions / memory_scopes / embeddings, so the plain single-column FKs
-- re-added at the end don't fail against orphaned references — and this stays correct if another
-- inbound FK is ever added. SET CONSTRAINTS flushes the deferred trigger the claims cascade queues
-- (claims.superseded_by is deferrable). Production tables are empty this phase; the cascade only
-- does work when a test reverses after inserting rows.
DELETE FROM memories;
SET CONSTRAINTS ALL IMMEDIATE;

ALTER TABLE claims DROP CONSTRAINT claims_memory_fk;
DROP INDEX claims_project_memory_idx;

ALTER TABLE memory_scopes DROP CONSTRAINT memory_scopes_memory_fk;
ALTER TABLE memory_scopes DROP CONSTRAINT memory_scopes_pkey;
ALTER TABLE memory_scopes DROP COLUMN project_id;
ALTER TABLE memory_scopes ADD PRIMARY KEY (memory_id, scope_type, scope_id);

ALTER TABLE memory_versions DROP CONSTRAINT memory_versions_memory_fk;
ALTER TABLE memory_versions DROP CONSTRAINT memory_versions_pkey;
ALTER TABLE memory_versions DROP COLUMN project_id;
ALTER TABLE memory_versions ADD PRIMARY KEY (memory_id, version);

DROP TABLE embeddings;
DROP TABLE memories CASCADE;

CREATE TABLE memories (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    kind             text        NOT NULL,
    content          text        NOT NULL,
    version          integer     NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL DEFAULT now(),
    entities         jsonb       NOT NULL DEFAULT '[]'::jsonb,
    valid_from       timestamptz NOT NULL DEFAULT now(),
    valid_to         timestamptz,
    superseded_by    uuid        REFERENCES memories (id) ON DELETE SET NULL,
    trust_tier       text        NOT NULL DEFAULT 'normal',
    review_status    text        NOT NULL DEFAULT 'auto_approved',
    created_by_agent text,
    source_event_id  uuid        REFERENCES events (id) ON DELETE SET NULL,
    scope_keys       text[]      NOT NULL DEFAULT '{}'::text[],
    CONSTRAINT memories_kind_check CHECK (kind IN ('working', 'episodic', 'semantic', 'procedural'))
);
CREATE INDEX memories_scope_keys_gin ON memories USING gin (scope_keys);

CREATE TABLE embeddings (
    project_id uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    memory_id  uuid        NOT NULL REFERENCES memories (id) ON DELETE CASCADE,
    model_id   text        NOT NULL,
    vec        vector      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (memory_id, model_id)
);
CREATE INDEX embeddings_project_id_idx ON embeddings (project_id);

ALTER TABLE memory_versions
    ADD CONSTRAINT memory_versions_memory_id_fkey FOREIGN KEY (memory_id) REFERENCES memories (id) ON DELETE CASCADE;
ALTER TABLE memory_scopes
    ADD CONSTRAINT memory_scopes_memory_id_fkey FOREIGN KEY (memory_id) REFERENCES memories (id) ON DELETE CASCADE;
ALTER TABLE claims
    ADD CONSTRAINT claims_memory_id_fkey FOREIGN KEY (memory_id) REFERENCES memories (id) ON DELETE CASCADE;
