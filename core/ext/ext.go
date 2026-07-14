// Package ext defines Lore's compile-time extension points: the small interfaces
// a downstream build swaps out to change authorization, conflict resolution, and
// metering without forking the core. Composition happens at compile time via
// core.NewServer options, not a runtime plugin system.
//
// Phase 0 ships the interfaces and their OSS default implementations only; the
// server composes them but does not yet invoke them on the request path. Each
// interface stays small (1-3 methods) with context.Context first; the error
// contracts live in errors.go.
package ext

import (
	"context"
	"encoding/json"
	"time"
)

// PolicyEngine authorizes an action against the scopes granted to a caller.
// OSS default: BasicScopePolicy (scope-tag match). A downstream build swaps in a
// rule engine with conditional and time-bound grants.
type PolicyEngine interface {
	// Authorize returns nil when scopes permit action, or ErrPermissionDenied
	// when they do not.
	Authorize(ctx context.Context, scopes []string, action string) error
}

// Adjudicator resolves a conflict between a stored value and an incoming write.
// OSS defaults: LWW and FieldMerge. A downstream build swaps in a more advanced
// resolution policy.
type Adjudicator interface {
	// Resolve returns the value that should survive the conflict, plus a short identifier of the policy
	// that decided (the write path records it with the resolution). Ordering is arrival order — Incoming
	// is the write being applied now, so a last-write-wins policy returns it. The per-side provenance is
	// for the recorded reason and for a policy that dispatches per project; it is never used to order the
	// two, because Seq is monotonic only within a run and so is not comparable across runs.
	Resolve(ctx context.Context, c Conflict) (Resolution, error)
}

// Conflict is two competing values for one subject. ProjectID scopes it to a tenant, so a policy can
// dispatch on the project's configuration. CurrentSource and IncomingSource say where each value came
// from — the run and its per-run sequence — for the recorded reason and audit trail, NOT for ordering:
// seq is monotonic only within a run, so cross-run seqs are not comparable, and the incoming value is the
// later arrival by construction (the caller applies writes in arrival order).
type Conflict struct {
	ProjectID      string
	Current        []byte
	Incoming       []byte
	CurrentSource  Provenance
	IncomingSource Provenance
}

// Provenance is where a conflicting value came from: the run and its per-run sequence. It labels a value
// for audit; it is not an ordering key (see Conflict).
type Provenance struct {
	RunID string
	Seq   int64
}

// Resolution is the surviving value chosen by an Adjudicator, with a short identifier of the policy that
// produced it (e.g. "last-write-wins", "field-merge") that the write path records alongside the change.
type Resolution struct {
	Value  []byte
	Reason string
}

// MeteringSink records a usage measurement. OSS default: NoopMetering — a
// self-hosted deployment reads usage from its local pack logs. A downstream build
// swaps in a usage-metering pipeline.
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

// Embedder turns memory content into embedding vectors for similarity search. The OSS default is
// FixtureEmbedder (deterministic, offline — no API key), so the read path can be built and tested end
// to end without a provider; a downstream build swaps in a real embedding model behind this same
// interface. Embed is batch-shaped so the write path can embed a whole consolidation pass in one call.
// Vectors are stored under ModelID, and reads query a single model space (a project's active model), so
// a project can carry embeddings from more than one model at once — e.g. during a model migration.
type Embedder interface {
	// Embed returns one vector per input text, in the same order, each of length Dim(). A provider or
	// transport failure returns an error and no partial result; the caller retries the pass.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dim is the fixed dimension of every vector Embed returns. The write path asserts each vector's
	// length equals it before storing, because a wrong-dimension vector in the dimensionless column
	// would otherwise surface only later, when the vector index is built.
	Dim() int
	// ModelID identifies the embedding model; embeddings are stored under it.
	ModelID() string
}

// Extractor distils a coalesced window of one run's events into candidate memories, claims, and
// entity mentions. It abstracts the extraction provider: the OSS defaults are FixtureExtractor
// (deterministic, offline — no API key) and, later, a thin Claude/OpenAI provider adapter behind
// this same interface with bring-your-own-key. Extraction is a batch call (one window, not one
// event) so the caller can coalesce many events into a single pass.
type Extractor interface {
	// Extract returns the candidates distilled from in.Events. A provider or transport failure
	// returns an error (e.g. ErrExtractorUnavailable) and no partial result; the caller retries.
	Extract(ctx context.Context, in ExtractInput) (ExtractResult, error)
}

// BatchExtractor is an optional capability an Extractor may also implement for latency-tolerant
// batch extraction: submit a window now, collect the distilled result later. It lets the coalesced
// job release its worker slot while the provider processes the window out of band (e.g. a Batch API
// with minutes of latency), trading freshness for cost. An Extractor that does not implement this
// runs only the synchronous Extract path. The two calls span separate job attempts, so the handle
// is the only state carried between them — the caller persists it and re-derives everything else.
type BatchExtractor interface {
	// SubmitBatch submits one window for asynchronous extraction and returns an opaque handle the
	// caller persists and later passes to CollectBatch. A provider or transport failure returns an
	// error (e.g. ErrExtractorUnavailable) and no handle; the caller retries the submission.
	SubmitBatch(ctx context.Context, in ExtractInput) (handle string, err error)
	// CollectBatch reports whether the batch named by handle has finished. When done is false the
	// result is empty and the caller polls again later; when done is true it returns the distilled
	// result. A provider or transport failure returns an error (e.g. ErrExtractorUnavailable).
	CollectBatch(ctx context.Context, handle string) (res ExtractResult, done bool, err error)
}

// ExtractInput is one extraction pass over a run's events. Events are ordered by Seq — extraction
// and provenance are keyed on Seq, never on a client clock.
type ExtractInput struct {
	ProjectID string
	RunID     string
	Events    []InputEvent
}

// InputEvent is a single event offered to the extractor: its per-run Seq (for ordering and
// provenance), the writing agent, and the opaque JSON payload.
type InputEvent struct {
	Seq     int64
	AgentID string
	Payload json.RawMessage
}

// ExtractResult is everything one pass distilled. Any slice may be empty.
type ExtractResult struct {
	Memories []CandidateMemory
	Claims   []CandidateClaim
	Entities []EntityMention
}

// CandidateMemory is a distilled memory awaiting persistence. The write path derives the stored
// provenance (source_event_id, created_by_agent) and defaults (trust tier) from SourceSeq.
type CandidateMemory struct {
	Kind      string // semantic | episodic | procedural
	Content   string
	SourceSeq int64 // the event this was distilled from
}

// CandidateClaim is a structured assertion about an entity. EventTime is set only for temporal
// claims; it never drives ordering (Seq does).
type CandidateClaim struct {
	Entity    string
	Predicate string
	Value     json.RawMessage
	EventTime *time.Time
	SourceSeq int64
}

// EntityMention is an entity the extractor recognised in the window.
type EntityMention struct {
	Name    string
	Type    string
	Aliases []string
}
