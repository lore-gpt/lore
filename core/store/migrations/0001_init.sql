-- +goose Up

-- Extensions shipped by the ParadeDB image (pgvector + pg_search). pgvector backs
-- future dense retrieval; pg_search backs BM25. Phase 0 only proves they load.
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_search;

CREATE TABLE organizations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE projects (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    key_hash   text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);

CREATE TABLE runs (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    status     text NOT NULL DEFAULT 'active',
    started_at timestamptz NOT NULL DEFAULT now()
);

-- Append-only raw event log. The write path (Phase 1) inserts here and enqueues
-- an extraction job in the same transaction.
CREATE TABLE events (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id     uuid NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    agent_id   text NOT NULL,
    payload    jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX events_run_id_idx ON events (run_id);

-- Phase 1 placeholder. The inline embedding column is intentionally
-- DIMENSIONLESS — no dimension is fixed before it is measured; Phase 1
-- relocates embeddings to a dedicated multi-model table.
CREATE TABLE memories (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    kind       text NOT NULL,
    content    text NOT NULL,
    embedding  vector,
    version    integer NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS memories;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS runs;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS organizations;
-- Extensions are cluster/database-wide and may be relied on outside these tables,
-- so the rollback intentionally leaves `vector` and `pg_search` installed.
