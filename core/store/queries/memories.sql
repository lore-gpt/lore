-- name: InsertMemory :one
-- Persist one distilled memory. project_id routes the row to its tenant partition, which must
-- already exist (memories is LIST-partitioned with no default partition, so an un-provisioned
-- project fails loud rather than silently mis-routing). Provenance — source_event_id and
-- created_by_agent — is resolved by the caller from the event the memory was distilled from;
-- source_event_id is nullable only for manual (non-extracted) writes. Everything else takes its
-- schema default: trust_tier 'normal', review_status 'auto_approved', version 1, valid_from now(),
-- empty entities/scope_keys — the single-schema basic behaviour the OSS build always writes.
INSERT INTO memories (project_id, kind, content, source_event_id, created_by_agent)
VALUES ($1, $2, $3, $4, $5)
RETURNING id;
