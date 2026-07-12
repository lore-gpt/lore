-- +goose Up

-- Database role separation. These roles are cluster-global, so they are created guarded (and
-- never dropped on Down — other databases in the cluster may share them).
--   lore_app      the application role: DML on tenant tables, and — being neither owner nor
--                 superuser — subject to the Row-Level Security added below.
--   lore_migrate  owns DDL (migrations); as table owner it bypasses RLS.
--   lore_readonly analytics SELECT. Cross-tenant analytics needs BYPASSRLS, which requires
--                 superuser to grant and is deferred to the app-role cutover; here it is a
--                 plain role so this migration runs under any owner.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'lore_app')      THEN CREATE ROLE lore_app NOLOGIN;      END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'lore_migrate')  THEN CREATE ROLE lore_migrate NOLOGIN;  END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'lore_readonly') THEN CREATE ROLE lore_readonly NOLOGIN; END IF;
END $$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO lore_app, lore_readonly;

-- lore_app is granted only the tenant tables (never organizations or goose bookkeeping), so a
-- leak is at most within a tenant and cross-tenant is caught by RLS. audit_log is INSERT + SELECT
-- only — no UPDATE/DELETE grant, the grant-level twin of its append-only trigger. Future tenant
-- tables must add their own grants + RLS (no blanket default privileges, to keep this list tight).
GRANT SELECT, INSERT, UPDATE, DELETE ON
    projects, api_keys, runs, events, memories, embeddings,
    memory_versions, memory_scopes, claims, entities, entity_links, pack_logs
    TO lore_app;
GRANT SELECT, INSERT ON audit_log TO lore_app;

GRANT SELECT ON
    projects, api_keys, runs, events, memories, embeddings,
    memory_versions, memory_scopes, claims, entities, entity_links, pack_logs, audit_log
    TO lore_readonly;

-- events joins the tenant-column club so it can be RLS-scoped. runs gains UNIQUE (project_id, id)
-- so events can carry a COMPOSITE foreign key (project_id, run_id) -> runs (project_id, id): an
-- event's project_id then cannot disagree with its run's project (the structural denormalization
-- guard migration 0006 used). The write path derives project_id from the run, so the pair is
-- always consistent; the composite FK is the backstop for any other insert path.
ALTER TABLE runs ADD CONSTRAINT runs_project_id_id_key UNIQUE (project_id, id);
-- Add the column nullable, backfill it from each event's run, then set NOT NULL. On the first
-- apply events is empty so the backfill is a no-op; on a re-apply after a Down (when events may
-- hold rows) this reconstructs project_id from runs instead of failing on a bare NOT NULL add.
ALTER TABLE events ADD COLUMN project_id uuid;
UPDATE events e SET project_id = r.project_id FROM runs r WHERE r.id = e.run_id;
ALTER TABLE events ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE events DROP CONSTRAINT events_run_id_fkey;
ALTER TABLE events ADD CONSTRAINT events_run_fk
    FOREIGN KEY (project_id, run_id) REFERENCES runs (project_id, id) ON DELETE CASCADE;
CREATE INDEX events_project_run_idx ON events (project_id, run_id);

-- Second belt: Row-Level Security. Each tenant-scoped table only shows and only accepts rows
-- whose project matches the session GUC lore.project_id. current_setting(..., true) returns NULL
-- when the GUC is unset and NULLIF maps an empty string to NULL, so "no project set" resolves to
-- (project_id = NULL) = no rows — fail-closed. WITH CHECK applies the same test to INSERT/UPDATE
-- so a write can never land in another tenant. RLS is ENABLED, not FORCED: the table owner and
-- superusers bypass it (migrations, analytics), while lore_app is subject to it. On the
-- partitioned parents (memories, embeddings) the policy propagates to every partition.

ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
CREATE POLICY projects_tenant ON projects
    USING (id = NULLIF(current_setting('lore.project_id', true), '')::uuid)
    WITH CHECK (id = NULLIF(current_setting('lore.project_id', true), '')::uuid);

ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY api_keys_tenant ON api_keys
    USING (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid)
    WITH CHECK (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid);

ALTER TABLE runs ENABLE ROW LEVEL SECURITY;
CREATE POLICY runs_tenant ON runs
    USING (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid)
    WITH CHECK (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid);

ALTER TABLE events ENABLE ROW LEVEL SECURITY;
CREATE POLICY events_tenant ON events
    USING (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid)
    WITH CHECK (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid);

ALTER TABLE memories ENABLE ROW LEVEL SECURITY;
CREATE POLICY memories_tenant ON memories
    USING (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid)
    WITH CHECK (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid);

ALTER TABLE embeddings ENABLE ROW LEVEL SECURITY;
CREATE POLICY embeddings_tenant ON embeddings
    USING (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid)
    WITH CHECK (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid);

ALTER TABLE memory_versions ENABLE ROW LEVEL SECURITY;
CREATE POLICY memory_versions_tenant ON memory_versions
    USING (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid)
    WITH CHECK (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid);

ALTER TABLE memory_scopes ENABLE ROW LEVEL SECURITY;
CREATE POLICY memory_scopes_tenant ON memory_scopes
    USING (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid)
    WITH CHECK (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid);

ALTER TABLE claims ENABLE ROW LEVEL SECURITY;
CREATE POLICY claims_tenant ON claims
    USING (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid)
    WITH CHECK (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid);

ALTER TABLE entities ENABLE ROW LEVEL SECURITY;
CREATE POLICY entities_tenant ON entities
    USING (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid)
    WITH CHECK (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid);

ALTER TABLE entity_links ENABLE ROW LEVEL SECURITY;
CREATE POLICY entity_links_tenant ON entity_links
    USING (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid)
    WITH CHECK (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid);

ALTER TABLE pack_logs ENABLE ROW LEVEL SECURITY;
CREATE POLICY pack_logs_tenant ON pack_logs
    USING (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid)
    WITH CHECK (project_id = NULLIF(current_setting('lore.project_id', true), '')::uuid);

-- +goose Down

-- Reverse to the pre-0008 shape: drop the policies and disable RLS, revert the events/runs
-- changes, and revoke the grants. Roles are cluster-global and may back other databases, so they
-- are NOT dropped here — only their privileges are revoked.
DROP POLICY IF EXISTS pack_logs_tenant ON pack_logs;         ALTER TABLE pack_logs DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS entity_links_tenant ON entity_links;   ALTER TABLE entity_links DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS entities_tenant ON entities;           ALTER TABLE entities DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS claims_tenant ON claims;               ALTER TABLE claims DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS memory_scopes_tenant ON memory_scopes; ALTER TABLE memory_scopes DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS memory_versions_tenant ON memory_versions; ALTER TABLE memory_versions DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS embeddings_tenant ON embeddings;       ALTER TABLE embeddings DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS memories_tenant ON memories;           ALTER TABLE memories DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS events_tenant ON events;               ALTER TABLE events DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS runs_tenant ON runs;                   ALTER TABLE runs DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS api_keys_tenant ON api_keys;           ALTER TABLE api_keys DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS projects_tenant ON projects;           ALTER TABLE projects DISABLE ROW LEVEL SECURITY;

ALTER TABLE events DROP CONSTRAINT events_run_fk;
DROP INDEX events_project_run_idx;
ALTER TABLE events DROP COLUMN project_id;
ALTER TABLE events ADD CONSTRAINT events_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE CASCADE;
ALTER TABLE runs DROP CONSTRAINT runs_project_id_id_key;

REVOKE ALL PRIVILEGES ON
    projects, api_keys, runs, events, memories, embeddings, memory_versions, memory_scopes,
    claims, entities, entity_links, pack_logs, audit_log
    FROM lore_app, lore_readonly;
REVOKE USAGE ON SCHEMA public FROM lore_app, lore_readonly;
