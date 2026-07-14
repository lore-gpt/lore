-- +goose Up

-- Complete api_keys for real authentication. The table shipped in 0001 with the shape a control plane would
-- write (project_id, key_hash, revoked_at); this makes it usable by the request path and by an operator:
--   UNIQUE (key_hash)  a bearer token is looked up by the hash of its raw value, so the hash must resolve to
--                      at most one key — the uniqueness that lets the lookup be a single indexed probe.
--   name               operator label for a key (nullable), so a human can tell keys apart.
--   key_prefix         the first several characters of the raw token (nullable, non-secret), stored so a key
--                      can be recognised in a listing without revealing the secret — and so a future secret
--                      scanner has a stable prefix to match. The secret itself is never stored; only its hash.
-- All additive and nullable (no backfill): the table is empty until a key is minted.
ALTER TABLE api_keys
    ADD COLUMN name       text,
    ADD COLUMN key_prefix text;

CREATE UNIQUE INDEX api_keys_key_hash_key ON api_keys (key_hash);

-- +goose Down

DROP INDEX IF EXISTS api_keys_key_hash_key;

ALTER TABLE api_keys
    DROP COLUMN key_prefix,
    DROP COLUMN name;
