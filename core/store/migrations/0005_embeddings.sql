-- +goose Up

-- Phase 1 relocates dense vectors into their own storage. Phase 0 shipped an inline
-- memories.embedding column and 0002 dropped it precisely so this could land properly:
-- embeddings live in a dedicated, dimensionless table keyed by model, because the
-- embedding model is not yet fixed and a project may re-embed under a new one. Reads
-- always query a single model's space (never a mix of dimensions).

-- One vector per (memory, model). vec is a dimensionless `vector` — no dimension is
-- pinned until the model choice is made, and different models may have different
-- dimensions. project_id is carried directly (not only via memory_id) so the table is
-- ready for the same per-project partitioning and row-level tenancy the other tenant
-- tables get in later migrations; when that partitioning lands (a table rebuild, since
-- Postgres can't partition in place) project_id joins the primary key, so the key is
-- (memory_id, model_id) here rather than widened prematurely.
CREATE TABLE embeddings (
    project_id uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    memory_id  uuid        NOT NULL REFERENCES memories (id) ON DELETE CASCADE,
    model_id   text        NOT NULL,
    vec        vector      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (memory_id, model_id)
);

-- Back the delete-cascade from projects (the memory_id cascade rides the primary key).
CREATE INDEX embeddings_project_id_idx ON embeddings (project_id);

-- The active embedding model for a project. Reads resolve a memory's vector in this
-- model's space; it is NULL until a model is chosen. A dedicated column keeps the
-- choice next to the project rather than in a side table.
ALTER TABLE projects ADD COLUMN active_model_id text;

-- Entities carry a single inline embedding (used later for entity linking / alias
-- resolution), distinct from the multi-model memory vectors above: an entity is
-- resolved in one space, so a per-model table would be over-modelling. Dimensionless
-- for the same reason as embeddings.vec. Deferred out of the previous migration so all
-- the vector machinery arrives together here.
ALTER TABLE entities ADD COLUMN embedding vector;

-- +goose Down

-- Reverse of Up.
ALTER TABLE entities DROP COLUMN IF EXISTS embedding;
ALTER TABLE projects DROP COLUMN IF EXISTS active_model_id;
DROP TABLE IF EXISTS embeddings;
