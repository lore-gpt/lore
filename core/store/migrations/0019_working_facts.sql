-- +goose Up

-- working_facts is the durable home of a run's working memory: one row per (run, subject), written
-- in the SAME transaction that appends the state event that carries it, and overwritten in place.
--
-- Why it exists. The context pack's working section was served only from a hot cache. The fallback
-- read durable memories of kind 'working', but nothing ever wrote one — so a deployment without the
-- cache had no working section at all, and a fact written while the cache was down never came back:
-- once extraction advanced the run checkpoint past its event, it left the raw tail too. The fact
-- survived elsewhere, but not on the surface agents actually read. This table is that missing
-- durable producer, and because the write shares the event's transaction, a caller that holds an
-- event's seq is guaranteed the fact behind it is committed too.
--
-- Deliberately NOT versioned and carrying no history. That is the point of the working lane: a
-- coordination value a run rewrites often, whose history already lives in `events` and is
-- recoverable by (project_id, run_id, seq). Retraction is not representable — the way to change a
-- fact is to write it again.
--
-- Column notes:
--   run_id      the key is RUN-scoped, not project-scoped. This is the single most likely
--               implementation mistake, because the neighbouring claims table keys the same-looking
--               subject as (project_id, entity_id, predicate). Copying that shape here would let one
--               run's write hide another run's own fact — the reason a claims-based design for this
--               section was rejected.
--   entity /
--   predicate   stored raw rather than resolved to an entity id: the ingest path must not pay a
--               registry round-trip, and the pack renders the strings directly.
--   seq         the event seq that last wrote this subject. It is the ordering authority for the
--               pack's rendering (freshest first) and the guard that keeps a replayed or
--               out-of-order write from overwriting a newer value.
--   agent_id    which agent asserted it; rendered alongside the fact as provenance.
--   updated_at  operator debugging only. NOT an ordering key (now() is transaction-start time, so
--               two transactions can commit out of clock order) and NOT a purge key: a subject that
--               has not changed in weeks may still be the current truth for a live run.
--
-- The primary key IS the upsert key and IS the read index: the pack's (project_id, run_id) lookup is
-- a prefix range scan over it. There is deliberately no second index — a run's working set is
-- bounded by its distinct subjects, so ordering a handful of rows is free, while a second index
-- would be maintained on every ingest. Because the key columns are never updated and no other index
-- exists, an overwrite is HOT-eligible: no index churn, dead tuples reclaimed in place.
--
-- There is deliberately no surrogate id. A working fact is a subject's current value, not an object with
-- identity and history; giving it a uuid would invite treating it as one. It also removes the temptation to
-- cite a working fact in the pack's source list, which is a list of memory ids — that exclusion is enforced
-- in the renderer, but there is nothing here for it to cite even if someone tried.
CREATE TABLE working_facts (
    project_id uuid        NOT NULL,
    run_id     uuid        NOT NULL,
    entity     text        NOT NULL,
    predicate  text        NOT NULL,
    value      jsonb       NOT NULL,
    seq        bigint      NOT NULL,
    agent_id   text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (project_id, run_id, entity, predicate),

    -- Composite FK, mirroring events_run_fk (migration 0008): a fact's project_id can never
    -- disagree with its run's project, and deleting a run — or the project above it — takes its
    -- working facts with it, which is what bounds this table's growth.
    CONSTRAINT working_facts_run_fk
        FOREIGN KEY (project_id, run_id) REFERENCES runs (project_id, id) ON DELETE CASCADE,

    -- seq is 1-based (0 covers nothing), so a non-positive value is a bug in the writer, not data.
    CONSTRAINT working_facts_seq_positive CHECK (seq > 0),

    -- octet_length, not length: the ingest validator bounds these in BYTES, and length() counts
    -- characters, which would leave a silent hole for any writer that bypasses it.
    CONSTRAINT working_facts_entity_len    CHECK (entity    <> '' AND octet_length(entity)    <= 256),
    CONSTRAINT working_facts_predicate_len CHECK (predicate <> '' AND octet_length(predicate) <= 256)

    -- No CHECK on value's size on purpose: the ingest cap is operator-configurable, so a fixed
    -- ceiling here would start rejecting legal requests the moment an operator raised it.
);

-- Tenant table: its own grants and RLS, as migration 0008 requires of every new one (no blanket
-- default privileges). DELETE is granted because the row is a mutable current value and an operator
-- clearing a run's working state is a legitimate action, matching the other tenant tables.
GRANT SELECT, INSERT, UPDATE, DELETE ON working_facts TO lore_app;
GRANT SELECT ON working_facts TO lore_readonly;

ALTER TABLE working_facts ENABLE ROW LEVEL SECURITY;
CREATE POLICY working_facts_tenant ON working_facts
    USING (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid)
    WITH CHECK (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid);

-- Note for the app-role cutover: the ingest handler writes this table on a plain pool transaction
-- that does not set lore.project_id, exactly as it already does for the events insert. Both work
-- today only because the pooled role owns the tables and bypasses RLS. Whoever moves the write path
-- onto lore_app must scope that transaction, or BOTH inserts start failing the WITH CHECK.
--
-- Nothing else may write this table without re-examining lock order. Ingest takes the run's row lock (in
-- InsertEvent) and then this table's row; the consolidation pass takes entity and claim rows and only then
-- the run's row. Adding a working_facts write to that pass BEFORE it advances the run checkpoint would
-- close the cycle and deadlock against ingest under concurrency.
--
-- Runs that already existed when this shipped start with no rows here; their working section fills in as
-- soon as they write a state fact again. There is no backfill on purpose. One is a single statement if an
-- operator wants it, and it is safe to run against live traffic precisely because the seq guard makes it
-- unable to overwrite a newer value — but the two type filters below are not optional:
--
--   INSERT INTO working_facts (project_id, run_id, entity, predicate, value, seq, agent_id)
--   SELECT DISTINCT ON (e.project_id, e.run_id, e.payload->>'entity', e.payload->>'predicate')
--          e.project_id, e.run_id, e.payload->>'entity', e.payload->>'predicate',
--          e.payload->'value', e.seq, e.agent_id
--     FROM events e
--    WHERE e.payload->>'kind' = 'state'
--      AND jsonb_typeof(e.payload->'entity')    = 'string'
--      AND jsonb_typeof(e.payload->'predicate') = 'string'
--      AND e.payload ? 'value'
--    ORDER BY e.project_id, e.run_id, e.payload->>'entity', e.payload->>'predicate', e.seq DESC
--   ON CONFLICT (project_id, run_id, entity, predicate) DO UPDATE
--      SET value = EXCLUDED.value, seq = EXCLUDED.seq, agent_id = EXCLUDED.agent_id, updated_at = now()
--    WHERE working_facts.seq < EXCLUDED.seq;
--
-- Why those filters. `->>` coerces, so a numeric entity would arrive as its text form — admitting a subject
-- the ingest validator would have rejected. And a state-shaped event MISSING a subject yields NULL, which
-- the NOT NULL column rejects, aborting the entire statement over one bad historical row instead of
-- skipping it. Such rows should not exist (the server validates state facts at ingestion) but may predate
-- that validation, so a backfill must not assume they do not.

-- +goose Down

-- Dropping the table takes its policy, grants, primary key and foreign key with it.
DROP TABLE IF EXISTS working_facts;
