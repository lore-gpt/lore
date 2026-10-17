package jobs

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/store/db"
)

// TestOccurredAtPrefersTheStatedTimeAndFallsBackQuietly pins where an event's date comes from: the payload's
// own RFC 3339 "occurred_at" when there is one (a client replaying history knows the original time), the
// server's receipt time otherwise — and never an error, because a date label is not worth failing a pass.
func TestOccurredAtPrefersTheStatedTimeAndFallsBackQuietly(t *testing.T) {
	received := time.Date(2026, time.October, 17, 21, 0, 0, 0, time.UTC)
	stated := time.Date(2023, time.May, 21, 14, 49, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		payload  string
		received bool
		want     time.Time
	}{
		{"a stated time wins over receipt", `{"occurred_at":"2023-05-21T14:49:00Z"}`, true, stated},
		{"a stated offset is normalised to UTC", `{"occurred_at":"2023-05-21T17:49:00+03:00"}`, true, stated},
		{"fractional seconds are accepted", `{"occurred_at":"2023-05-21T14:49:00.250Z"}`, true,
			stated.Add(250 * time.Millisecond)},
		{"a malformed stated time falls back to receipt", `{"occurred_at":"21 May 2023"}`, true, received},
		{"a non-string stated time falls back to receipt", `{"occurred_at":1684680540}`, true, received},
		{"a payload that is not an object falls back to receipt", `"just text"`, true, received},
		{"no stated time is dated by receipt", `{"content":"hello"}`, true, received},
		{"neither stated nor received is undated", `{"content":"hello"}`, false, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := db.Event{Payload: []byte(tc.payload)}
			if tc.received {
				e.CreatedAt = pgtype.Timestamptz{Time: received.In(time.FixedZone("UTC+3", 3*60*60)), Valid: true}
			}
			got := occurredAt(e)
			if !got.Equal(tc.want) {
				t.Errorf("occurredAt = %v, want %v", got, tc.want)
			}
			if !got.IsZero() && got.Location() != time.UTC {
				t.Errorf("occurredAt location = %v, want UTC", got.Location())
			}
		})
	}
}
