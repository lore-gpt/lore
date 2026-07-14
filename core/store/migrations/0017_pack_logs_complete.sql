-- +goose Up

-- Complete pack_logs. The 0007 shape was minimal (query, covered_seq, freshness_lag_ms, latency_ms) with a
-- note that the pack response would firm up later and finish this table; it has. All columns are additive and
-- nullable so the migration is a pure widen with no backfill:
--   scopes            the retrieval scope filter the pack ran under (empty/NULL = project-wide).
--   token_budget      the caller's requested token budget, NULL when the pack was unbounded.
--   est_source_tokens, packed_tokens
--                     the raw token-count ingredients — the estimated size of the material the pack drew from
--                     and the size of what it emitted. They are stored raw, on purpose: a downstream metering
--                     pass defines tokens_saved from them with its own (real-tokenizer) accounting, so this
--                     increment bakes in no ad-hoc saved-tokens formula and ships tokens_saved NULL.
--   tokens_saved      the metered saving; NULL here, filled by that later pass.
--   memory_ids        the ordered ids of the distilled memories that composed the pack (pack order), the
--                     substrate a run-trace view reads to reconstruct what a pack contained.
--   pack_hash         reserved for a byte-stable pack digest a later increment computes; NULL until then, so
--                     no consumer may assume it is present.
ALTER TABLE pack_logs
    ADD COLUMN scopes            text[],
    ADD COLUMN token_budget      integer,
    ADD COLUMN est_source_tokens integer,
    ADD COLUMN packed_tokens     integer,
    ADD COLUMN tokens_saved      integer,
    ADD COLUMN memory_ids        uuid[],
    ADD COLUMN pack_hash         bytea;

-- +goose Down

-- Reverse of Up (columns dropped in reverse add order).
ALTER TABLE pack_logs
    DROP COLUMN pack_hash,
    DROP COLUMN memory_ids,
    DROP COLUMN tokens_saved,
    DROP COLUMN packed_tokens,
    DROP COLUMN est_source_tokens,
    DROP COLUMN token_budget,
    DROP COLUMN scopes;
