// Package workmem is Lore's run-scoped working-memory store for hot coordination facts: values a run
// updates too often to version in Postgres (an event whose payload kind is "state"). A hot fact lives in
// a fast cache (Valkey) keyed by (project, run, entity, predicate), so a same-run reader sees the latest
// value immediately — the synchronous half of the read-your-writes guarantee — while Postgres stays the
// durable authority. The stripe is optional: when it is not configured, hot facts fall through to normal
// (durable, versioned) extraction instead.
package workmem

import (
	"context"
	"strconv"
	"strings"
)

// Mode is the operational state of the working-memory stripe. The extraction gate and the health endpoint
// both read it: only a Healthy store owns hot facts (they skip extraction); Disabled or Degraded means the
// facts fall through to durable extraction so nothing is lost.
type Mode int

const (
	// Disabled: no store is configured (LORE_VALKEY_URL unset). Set/Get are no-ops.
	Disabled Mode = iota
	// Healthy: the store is reachable and owns hot facts.
	Healthy
	// Degraded: configured but currently unreachable; callers fall back to durable extraction.
	Degraded
)

// String renders the mode for logs and the health endpoint.
func (m Mode) String() string {
	switch m {
	case Healthy:
		return "ok"
	case Degraded:
		return "degraded"
	default:
		return "disabled"
	}
}

// Key identifies one hot fact within a run: the subject (entity, predicate) it asserts.
type Key struct {
	ProjectID string
	RunID     string
	Entity    string
	Predicate string
}

// Value is a hot fact's current value with the provenance a reader needs: the run seq it was written at
// (freshness, like the rest of the write path) and the writing agent.
type Value struct {
	Value []byte `json:"v"`
	Seq   int64  `json:"s"`
	Agent string `json:"a"`
}

// Entry is one subject's hot value within a run — what GetAll returns so the read side never sees the
// internal field encoding.
type Entry struct {
	Entity    string
	Predicate string
	Value     Value
}

// Store is the working-memory port. Implementations: the Valkey-backed store (production), an in-memory
// store (tests), and the no-op disabled store. It speaks only domain terms — no cache-client types leak
// through — so the backing store is swappable in one file.
type Store interface {
	// Set writes (or overwrites) the hot value for a subject, refreshing the run's idle TTL. It is a
	// best-effort side-effect: an error is for the caller to log and count, never to fail ingestion.
	Set(ctx context.Context, k Key, v Value) error
	// Get returns the current hot value for a subject and whether one is present.
	Get(ctx context.Context, k Key) (Value, bool, error)
	// GetAll returns every hot value in a run (its working section) for the read side to merge. Empty when
	// the run has no working memory.
	GetAll(ctx context.Context, projectID, runID string) ([]Entry, error)
	// Mode reports the store's current operational state.
	Mode() Mode
	// Close releases the store's resources.
	Close()
}

// field encodes a subject as a hash field, length-prefixing the entity so the split is unambiguous for
// ANY bytes (a naive separator collides when the separator appears inside a name). parseField is its
// inverse; the encoding is internal — GetAll returns decoded Entry values.
func field(entity, predicate string) string {
	return strconv.Itoa(len(entity)) + ":" + entity + predicate
}

// parseField inverts field, recovering (entity, predicate). ok is false for a field not produced by
// field (a defensive guard against a malformed cache entry).
func parseField(f string) (entity, predicate string, ok bool) {
	i := strings.IndexByte(f, ':')
	if i <= 0 {
		return "", "", false
	}
	n, err := strconv.Atoi(f[:i])
	if err != nil || n < 0 || i+1+n > len(f) {
		return "", "", false
	}
	return f[i+1 : i+1+n], f[i+1+n:], true
}
