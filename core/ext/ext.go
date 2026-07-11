// Package ext defines Lore's compile-time extension points: the small interfaces
// a downstream build — including the closed-source cloud build — swaps out to
// change authorization, conflict resolution, and metering without forking the
// core. Composition happens at compile time via core.NewServer options, not a
// runtime plugin system.
//
// Phase 0 ships the interfaces and their OSS default implementations only; the
// server composes them but does not yet invoke them on the request path. Each
// interface stays small (1-3 methods) with context.Context first; the error
// contracts live in errors.go.
package ext

import (
	"context"
	"time"
)

// PolicyEngine authorizes an action against the scopes granted to a caller.
// OSS default: BasicScopePolicy (scope-tag match). The cloud build swaps in a
// rule engine with conditional and time-bound grants.
type PolicyEngine interface {
	// Authorize returns nil when scopes permit action, or ErrPermissionDenied
	// when they do not.
	Authorize(ctx context.Context, scopes []string, action string) error
}

// Adjudicator resolves a conflict between a stored value and an incoming write.
// OSS defaults: LWW and FieldMerge. The cloud build swaps in an LLM adjudicator
// or a manual-review queue.
type Adjudicator interface {
	// Resolve returns the value that should survive the conflict.
	Resolve(ctx context.Context, c Conflict) (Resolution, error)
}

// Conflict is two competing versions of the same opaque value, with the time
// each was written so a last-write-wins policy can order them.
type Conflict struct {
	Current    []byte
	Incoming   []byte
	CurrentAt  time.Time
	IncomingAt time.Time
}

// Resolution is the surviving value chosen by an Adjudicator.
type Resolution struct {
	Value []byte
}

// MeteringSink records a usage measurement. OSS default: NoopMetering — a
// self-hosted deployment reads usage from its local pack logs. The cloud build
// swaps in a billing usage pipeline.
type MeteringSink interface {
	// Record reports one usage measurement. It must not block the request path;
	// implementations buffer or drop rather than wait.
	Record(ctx context.Context, m Measurement) error
}

// Measurement is a single metered unit of work, e.g. {Op: "recall", Count: 1}.
type Measurement struct {
	Op    string
	Count int64
}
