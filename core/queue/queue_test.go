package queue

import (
	"context"
	"testing"
)

// An insert-only client (the shape New returns) must refuse to Start, so the
// server can never silently turn into a worker. This locks the guard in CI
// rather than a comment. No database needed: Start rejects before touching it.
func TestInsertOnlyClientCannotStart(t *testing.T) {
	q := &Queue{} // worker == false, i.e. what New produces
	if err := q.Start(context.Background()); err == nil {
		t.Error("Start() on an insert-only client returned nil, want an error")
	}
}
