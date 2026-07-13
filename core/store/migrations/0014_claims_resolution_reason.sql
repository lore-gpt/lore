-- +goose Up

-- resolution_reason records WHY a claim was superseded: the conflict policy that decided plus the
-- provenance of the winning and losing claims (ids and their run/seq). It is written on the SUPERSEDED
-- (losing) row — the one whose state changed — so a claim's supersession carries an audit trail of the
-- decision, not just the pointer to its replacement. The write path fills it when a policy resolves a
-- conflict; NULL for a claim that has not been superseded, or one superseded before this column existed.
-- Memory-changing resolutions record their reason in memory_versions.reason instead (the same principle:
-- the reason lives on the object that changed).
ALTER TABLE claims ADD COLUMN resolution_reason text;

-- +goose Down

ALTER TABLE claims DROP COLUMN resolution_reason;
