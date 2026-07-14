-- name: InsertMemory :one
-- Persist one distilled memory. project_id routes the row to its tenant partition, which must
-- already exist (memories is LIST-partitioned with no default partition, so an un-provisioned
-- project fails loud rather than silently mis-routing). Provenance — source_event_id and
-- created_by_agent — is resolved by the caller from the event the memory was distilled from;
-- source_event_id is nullable only for manual (non-extracted) writes. content_hash is the dedup
-- fingerprint (a hash of the kind, entity context, and normalized content) the consolidation path probes
-- on; NULL only for a path that opts out of dedup. Everything else takes its schema default: trust_tier 'normal',
-- review_status 'auto_approved', version 1, valid_from now(), empty entities/scope_keys — the
-- single-schema basic behaviour the OSS build always writes. context_hash is the entity-bucket key (the
-- content-less twin of content_hash) the near-duplicate probe groups on; NULL only for a path that opts
-- out of dedup.
INSERT INTO memories (project_id, kind, content, source_event_id, created_by_agent, content_hash, context_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id;

-- name: FindActiveMemoryByContentHash :one
-- The live memory in a project with a given content fingerprint, if any — the dedup probe the
-- consolidation path runs before inserting a distilled memory. Returns its id, version, and content so a
-- merge can bump the version and snapshot the memory's retained content into memory_versions. Scoped to
-- live rows by the same predicate as memories_content_hash_idx, so the partial index serves it and
-- superseded/expired history never matches. At most one row is live per fingerprint in practice (the path
-- merges rather than inserting a second), but the query does not rely on that: it returns the lowest id
-- so the choice is deterministic. No match returns pgx.ErrNoRows, the caller's "insert fresh" signal.
SELECT id, version, content
FROM memories
WHERE project_id = $1 AND content_hash = $2
  AND superseded_by IS NULL AND valid_to IS NULL
ORDER BY id
LIMIT 1;

-- name: IncrementMemoryVersion :one
-- Bump a live memory's version when the consolidation path merges a duplicate restatement into it
-- instead of inserting a new row, returning the new version number. The caller writes a matching
-- memory_versions row (new version number, the memory's retained content, the reason, the re-observing
-- agent) in the same transaction, so the live row's version and the latest memory_versions row stay in
-- lock-step. Exact-content dedup leaves the content unchanged; a later increment that merges differing
-- content updates the live content to match the version it snapshots. Project-scoped.
UPDATE memories
SET version = version + 1
WHERE project_id = $1 AND id = $2
RETURNING version;

-- name: ReadMemoryContentForUpdate :one
-- Lock a live memory's row and read its current content, taken in the same transaction immediately before
-- UpdateMemoryOnNearMerge overwrites the row, so a near-merge snapshots the content that was live just
-- before it — read UNDER the row lock (SELECT ... FOR UPDATE). A concurrent near-merge of the same memory
-- (possible only in the unserialised empty-entity-context bucket) cannot make that snapshot stale: this
-- blocks until the other commits, then reads its committed content. Project-scoped.
SELECT content FROM memories WHERE project_id = $1 AND id = $2 FOR UPDATE;

-- name: UpdateMemoryOnNearMerge :one
-- Overwrite a live memory's content when the consolidation path merges a NEAR-duplicate (embedding
-- similarity above the merge threshold, not an exact restatement) into it: the incoming write supersedes
-- the stored one (arrival-order last-write-wins — the same policy claims and working memory use), so the
-- live content, its exact fingerprint, and its provenance become the incoming memory's, and the version is
-- bumped, returning the new version. The caller has already locked and read the prior content via
-- ReadMemoryContentForUpdate in the same transaction, snapshots it into memory_versions, and re-stores the
-- incoming embedding. context_hash is unchanged in value (a near-duplicate is in the same entity bucket by
-- construction) but is set from the incoming memory too, so the whole row is written from one source.
-- Project-scoped.
UPDATE memories
SET content = $3, content_hash = $4, context_hash = $5, source_event_id = $6, created_by_agent = $7,
    version = version + 1
WHERE project_id = $1 AND id = $2
RETURNING version;
