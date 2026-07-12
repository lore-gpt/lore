-- +goose Up

-- The write path now assigns events.seq on every insert (a data-modifying CTE bumps the run's
-- last_seq and stamps the value onto the event), so the column can carry its NOT NULL guarantee.
-- Backfill first, then tighten the constraint.
--
-- Any event written before the seq-aware write path has seq IS NULL. Number those per run,
-- continuing past that run's current highest seq, ordered by write time (id breaks ties), so the
-- assigned values stay unique under UNIQUE (run_id, seq) and monotonic within the run. A fresh
-- install has no such rows, so this update touches nothing.
UPDATE events e
SET seq = s.new_seq
FROM (
    SELECT e2.project_id,
           e2.id,
           mx.base + row_number() OVER (PARTITION BY e2.run_id ORDER BY e2.created_at, e2.id) AS new_seq
    FROM events e2
    JOIN (SELECT run_id, COALESCE(max(seq), 0) AS base FROM events GROUP BY run_id) mx
      ON mx.run_id = e2.run_id
    WHERE e2.seq IS NULL
) s
WHERE e.project_id = s.project_id AND e.id = s.id;

-- Advance each run's counter to the highest seq now present so later inserts continue gap-free.
UPDATE runs r
SET last_seq = m.max_seq
FROM (SELECT run_id, max(seq) AS max_seq FROM events GROUP BY run_id) m
WHERE r.id = m.run_id AND r.last_seq < m.max_seq;

ALTER TABLE events ALTER COLUMN seq SET NOT NULL;

-- +goose Down

-- Loosen the column back to nullable. Backfilled seq values are left in place — they are valid
-- and there is no record of which rows were originally NULL.
ALTER TABLE events ALTER COLUMN seq DROP NOT NULL;
