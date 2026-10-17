-- +goose Up

-- occurred_at is when the event a memory was distilled from happened. A memory's text says what is true;
-- it rarely says when, and the two words that most often stand in for a date — "today", "recently" — mean
-- nothing once they are read weeks later. A reader asking how long ago something happened, or which of two
-- values came last, needs the date as data rather than hoping the content carries it.
--
-- It is the SOURCE event's time, never one a model produced: the write path takes it from the event's
-- payload when the client stated one ("occurred_at", RFC 3339 — a client replaying history knows when each
-- event originally happened) and otherwise from when the server received the event. A near-duplicate merge
-- rewrites it along with the content and provenance it replaces, so the row stays dated by the event it now
-- cites; an exact restatement leaves it, as it leaves the content.
--
-- NULL for a memory written without a source event and for every memory distilled before this column
-- existed; readers render those undated rather than guessing.
ALTER TABLE memories ADD COLUMN occurred_at timestamptz;

-- +goose Down

ALTER TABLE memories DROP COLUMN occurred_at;
