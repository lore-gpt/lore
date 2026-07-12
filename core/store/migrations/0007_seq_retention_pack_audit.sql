-- +goose Up

-- Per-run monotonic sequence. Each run owns a counter; the write path bumps it with
-- UPDATE runs SET last_seq = last_seq + 1 RETURNING (a single-row update whose row lock
-- serialises concurrent writers, so the sequence is gap-free without an advisory lock) and
-- stamps the returned value onto the event. events.seq is nullable for now because the
-- write path that assigns it lands in a later increment; it becomes NOT NULL then. The
-- UNIQUE (run_id, seq) both enforces one row per sequence number in a run and backs the
-- raw-tail range scan (events after a covered_seq), so no separate index is needed. NULLs
-- are distinct under UNIQUE, so pre-assignment events do not collide.
ALTER TABLE runs ADD COLUMN last_seq bigint NOT NULL DEFAULT 0;

ALTER TABLE events ADD COLUMN seq bigint;
ALTER TABLE events ADD CONSTRAINT events_run_seq_key UNIQUE (run_id, seq);

-- Per-project retention windows (NULL = keep indefinitely). Phase 1 ships the columns; the
-- purge jobs that read them arrive later. Single-schema principle: the knobs exist from day
-- one, so turning retention on is data, not a migration.
ALTER TABLE projects
    ADD COLUMN retain_events_days   integer,
    ADD COLUMN retain_memories_days integer;

-- pack_logs: one row per context-pack request, for run trace and observability. This is the
-- minimal shape; the pack response firms up in a later increment and completes this table
-- (scopes, token budget, tokens saved, the memory ids that composed the pack, pack hash).
-- run_id is ON DELETE SET NULL so a pack's trace row outlives the run it was taken in.
CREATE TABLE pack_logs (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    run_id           uuid        REFERENCES runs (id) ON DELETE SET NULL,
    query            text        NOT NULL,
    covered_seq      bigint,
    freshness_lag_ms integer,
    latency_ms       integer,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX pack_logs_project_created_idx ON pack_logs (project_id, created_at);

-- audit_log: append-only trail (policy changes, key rotation, GDPR erasure records...).
-- project_id is a bare uuid with NO foreign key on purpose: an erasure record must outlive
-- the project whose deletion it records, so it must not cascade away with that project.
CREATE TABLE audit_log (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid,
    actor      text        NOT NULL,
    action     text        NOT NULL,
    target     text,
    detail     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_project_created_idx ON audit_log (project_id, created_at);

-- Append-only is enforced in the database, not by convention. A row-level trigger rejects
-- UPDATE and DELETE; a statement-level trigger rejects TRUNCATE (a row trigger never sees
-- TRUNCATE). This blocks any principal that is neither the table owner nor a superuser: the
-- owner (today, the migration role) can still ALTER TABLE ... DISABLE TRIGGER, and a
-- superuser can bypass via session_replication_role — both are the documented, audited
-- escape for a genuine migration. The later grants that revoke write privileges from the
-- application role are the durable second belt, closing even an owner's quiet-tamper path.
-- +goose StatementBegin
CREATE FUNCTION audit_log_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only: % is not permitted', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_log_no_update_delete
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();

CREATE TRIGGER audit_log_no_truncate
    BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();

-- +goose Down

-- Drop the audit guards before the table (dropping the table would take its triggers, but
-- not the shared function). Order is the reverse of Up throughout.
DROP TRIGGER IF EXISTS audit_log_no_truncate ON audit_log;
DROP TRIGGER IF EXISTS audit_log_no_update_delete ON audit_log;
DROP FUNCTION IF EXISTS audit_log_append_only();
DROP TABLE IF EXISTS audit_log;

DROP TABLE IF EXISTS pack_logs;

ALTER TABLE projects
    DROP COLUMN retain_memories_days,
    DROP COLUMN retain_events_days;

ALTER TABLE events DROP CONSTRAINT events_run_seq_key;
ALTER TABLE events DROP COLUMN seq;

ALTER TABLE runs DROP COLUMN last_seq;
