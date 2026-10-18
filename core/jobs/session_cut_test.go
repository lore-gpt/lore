package jobs

import (
	"testing"

	"github.com/lore-gpt/lore/core/store/db"
)

func sessionEvents(sessions ...string) []db.Event {
	events := make([]db.Event, len(sessions))
	for i, s := range sessions {
		payload := `{"content":"x"}`
		if s != "" {
			payload = `{"content":"x","session_id":"` + s + `"}`
		}
		events[i] = db.Event{Seq: int64(i + 1), Payload: []byte(payload)}
	}
	return events
}

func TestSessionCutKeepsConversationsWhole(t *testing.T) {
	for _, tc := range []struct {
		name          string
		sessions      []string
		stillArriving bool
		want          int
	}{
		{"the last session may continue: it waits for the next pass", []string{"a", "a", "b", "b", "b"}, true, 2},
		{"nothing more is arriving: the last session is complete", []string{"a", "a", "b", "b", "b"}, false, 0},
		{"one session fills the window: the cap splits it as before", []string{"a", "a", "a"}, true, 0},
		{"no session ids: the window is kept as read", []string{"", "", ""}, true, 0},
		{"the last event names no session: nothing to keep whole", []string{"a", "b", ""}, true, 0},
		{"events before the first session stay in this pass", []string{"", "b", "b"}, true, 1},
		{"a single event is never cut", []string{"a"}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionCut(sessionEvents(tc.sessions...), tc.stillArriving); got != tc.want {
				t.Errorf("sessionCut(%q, %v) = %d, want %d", tc.sessions, tc.stillArriving, got, tc.want)
			}
		})
	}
}
