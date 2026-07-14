-- +goose Up

-- Full-text search substrate for the lexical retrieval leg: an expression GIN index over an English
-- to_tsvector of each memory's content. memories is LIST-partitioned by project_id, so this index on the
-- partitioned parent is a template — Postgres attaches a matching per-partition index to every existing and
-- future partition automatically (the same mechanism as memories_scope_keys_gin), so the lexical leg needs
-- no per-partition build machinery of its own. The read path's match predicate and rank reuse the identical
-- to_tsvector('english', content) expression so the planner can use this index; a drift between the two
-- would silently demote the match to a sequential scan. The text-search configuration is pinned to a literal
-- 'english' because an expression index requires an immutable expression (the one-argument to_tsvector,
-- which reads the session's default configuration, is only stable) — and the read query pins the same one.
CREATE INDEX memories_content_fts_gin ON memories USING gin (to_tsvector('english', content));

-- +goose Down
DROP INDEX memories_content_fts_gin;
