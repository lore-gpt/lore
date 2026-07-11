-- +goose Up

-- Phase 1 lands the entity graph's storage: an entity registry and the bi-temporal
-- links between entities. This is schema only — extraction writes edges and the
-- traversal/query surface open in later phases. OSS carries the tables from day one
-- so a later phase's graph is populated, not empty, the moment its readers turn on.

-- entities is the node table: one row per distinct thing a project's memories talk
-- about (an agent, a service, a task, a decision, ...). `type` is a free-text kind
-- for now; a project-scoped type registry that constrains it arrives in a later
-- increment. `aliases` and `degree_cached` back alias resolution and hub handling
-- that later phases fill in — they ship now (default empty / zero) so those steps are
-- data changes, not migrations. The dense `embedding` column lands with the rest of
-- the vector machinery in the following migration.
CREATE TABLE entities (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name          text        NOT NULL,
    type          text        NOT NULL,
    aliases       text[]      NOT NULL DEFAULT '{}'::text[],
    degree_cached integer     NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Entity lookup by name within a project (the entry point for linking a query's
-- mentions to nodes). Also covers the project_id prefix for the delete cascade.
CREATE INDEX entities_project_name_idx ON entities (project_id, name);

-- entity_links is the edge table: a directed, typed relationship (src -predicate-> dst)
-- that lives bi-temporally, exactly like memories and claims.
--   weight          relationship strength (observation count blended with recency);
--                   traversal orders neighbours by it.
--   trust_tier      numeric provenance rank (1 = default; higher is more trusted).
--                   Deliberately an ordinal here, distinct from the named governance
--                   vocabulary memories/claims carry under the same column name, because
--                   traversal ranks edges numerically rather than gating on a category.
--   valid_from/to   event-time validity window; valid_to IS NULL means the edge is
--                   currently true. Superseding an edge closes it (sets valid_to) and
--                   opens the replacement. This — not superseded_by — is what
--                   el_current_uq (below) keys "current" on.
--   superseded_by   the history spine: chains a closed edge to the one that replaced it.
--                   DEFERRABLE INITIALLY DEFERRED so an edge can be re-stated in one
--                   transaction — close the old (set valid_to) and insert the new it
--                   points at, the self-reference validated at commit. ON DELETE is
--                   NO ACTION so the chain keeps its referential integrity: a superseding
--                   edge can't be deleted out from under the edge pointing at it, so
--                   pruning an individual historical edge must be chain-aware; a whole-
--                   tenant delete that removes the entire chain in one transaction still
--                   commits (the check is deferred). Unlike claims — whose active set IS
--                   keyed on superseded_by — here superseded_by never affects "current",
--                   so NO ACTION buys chain integrity, not an active-set guarantee.
--   provenance      source memory/claim ids the edge was derived from (append-only).
--                   NOT NULL with no default: an edge must name where it came from.
--   first/last_seen_seq  ingestion-sequence bounds, so the graph can speak to freshness
--                   the same way the rest of the write path does. Ordering is the run
--                   sequence, never client clocks.
-- Self-loops (src = dst) are intentionally not constrained — some predicates may be
-- reflexive and the write path filters nonsense; only self-supersession is forbidden.
CREATE TABLE entity_links (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id     uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    src_entity_id  uuid        NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    dst_entity_id  uuid        NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    predicate      text        NOT NULL,
    weight         real        NOT NULL DEFAULT 1,
    trust_tier     smallint    NOT NULL DEFAULT 1,
    valid_from     timestamptz NOT NULL,
    valid_to       timestamptz,
    superseded_by  uuid        REFERENCES entity_links (id) ON DELETE NO ACTION
                               DEFERRABLE INITIALLY DEFERRED,
    provenance     uuid[]      NOT NULL,
    first_seen_seq bigint      NOT NULL,
    last_seen_seq  bigint      NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT entity_links_no_self_supersede CHECK (superseded_by IS NULL OR superseded_by <> id)
);

-- At most one current edge per (project, src, predicate, dst): a re-observation of the
-- same relationship updates in place, and a superseded edge (valid_to set) drops out so
-- history accumulates freely. The DB owns this invariant, not the write path.
CREATE UNIQUE INDEX el_current_uq
    ON entity_links (project_id, src_entity_id, predicate, dst_entity_id)
    WHERE valid_to IS NULL;

-- Back the delete-cascade from the two entity foreign keys and the tenancy key. The
-- covering out/in traversal indexes belong with the traversal engine in a later phase.
CREATE INDEX entity_links_src_idx        ON entity_links (src_entity_id);
CREATE INDEX entity_links_dst_idx        ON entity_links (dst_entity_id);
CREATE INDEX entity_links_project_id_idx ON entity_links (project_id);

-- +goose Down

-- Reverse of Up: drop the edge table (it references entities) before the node table.
DROP TABLE IF EXISTS entity_links;
DROP TABLE IF EXISTS entities;
