package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/lore-gpt/lore/core/store/db"
)

// errCheckpointConflict signals that the run's extraction checkpoint moved between the time this pass
// read it and the time it tried to advance it — another pass for the same run committed the same window
// first. The persister returns it so the transaction rolls back (dropping this pass's duplicate writes),
// and the worker treats it as a clean no-op: the concurrent winner owns the window and its tail. This is
// how a concurrent double-delivery yields exactly one set of memories and one checkpoint advance without
// a per-row idempotency key — the compare-and-swap on the stream checkpoint is the idempotency boundary.
var errCheckpointConflict = errors.New("extract_run: checkpoint advanced concurrently")

// advisoryLockWarnThreshold is how long a pass may wait to acquire its entity locks before the wait is
// logged as a warning: sustained waits past it are the signal that lock contention is high and the lock
// granularity may need to be finer (it is per-entity today).
const advisoryLockWarnThreshold = 250 * time.Millisecond

// normContentVersion identifies the content-normalization scheme folded into a dedup fingerprint. It is
// mixed into the hash preimage, so changing the scheme changes every fingerprint: a memory normalized
// under an old scheme can never silently merge against one normalized under a new scheme. Bump it
// whenever normalizeContent changes.
const normContentVersion = "1"

// normalizeContent folds a memory's content to the canonical form exact-content dedup compares on. The
// scheme is deliberately conservative — it collapses only differences that never change meaning —
// because the fingerprint decides merges and a wrong merge is unrecoverable (a distinct memory is lost),
// whereas a missed duplicate is harmless. It applies, in order: lower-casing; collapsing every run of
// Unicode whitespace to a single space with the ends trimmed; and stripping trailing sentence
// punctuation (and any space it exposes). It deliberately does NOT touch interior punctuation, quoting,
// or word order — anything that could distinguish two genuinely different statements.
func normalizeContent(s string) string {
	s = strings.ToLower(s)
	s = strings.Join(strings.Fields(s), " ") // collapse internal whitespace runs and trim both ends
	s = strings.TrimRight(s, ".!?,;: ")      // drop trailing sentence punctuation and any exposed space
	return s
}

// hashField writes a length-prefixed string into h so a fingerprint preimage is unambiguous: no two
// distinct field sequences can collide by field-boundary aliasing.
func hashField(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	h.Write(n[:])
	h.Write([]byte(s))
}

// writeContextFingerprint writes the entity-context prefix of a dedup fingerprint into h: the
// normalization version, the kind, and the sorted entity names with their count written first. It is
// shared by contextFingerprint and contentFingerprint so both hashes are built by one encoder over one
// version constant — they rotate together when the scheme changes, and a context_hash is always a strict
// prefix-hash of the corresponding content_hash's preimage. Folding kind and the entity context in keeps
// dedup INSIDE an entity bucket: identical text in a different context stays a separate memory.
func writeContextFingerprint(h hash.Hash, kind string, entityNames []string) {
	names := append([]string(nil), entityNames...)
	sort.Strings(names)
	hashField(h, normContentVersion)
	hashField(h, kind)
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(names)))
	h.Write(count[:])
	for _, n := range names {
		hashField(h, n)
	}
}

// contextFingerprint is the entity-bucket key for near-duplicate dedup: a hash over a memory's kind and
// entity context, WITHOUT its content. Live memories sharing a contextFingerprint are the candidate
// bucket the similarity probe compares a new memory against. Entity-less memories share one empty-context
// bucket. It is the content-less prefix of contentFingerprint.
func contextFingerprint(kind string, entityNames []string) []byte {
	h := sha256.New()
	writeContextFingerprint(h, kind, entityNames)
	return h.Sum(nil)
}

// contentFingerprint is the exact-dedup key: the entity-context prefix plus the normalized content.
// Exact fingerprint equality means the inputs are identical after normalization — never mere similarity —
// so it cannot cause a near-duplicate false merge; the vector-similarity probe handles near duplicates.
func contentFingerprint(kind string, entityNames []string, content string) []byte {
	h := sha256.New()
	writeContextFingerprint(h, kind, entityNames)
	hashField(h, normalizeContent(content))
	return h.Sum(nil)
}

// entityNames is the distinct set of entity natural keys a pass touches: the entities it mentions plus
// the subjects of its claims (a claim subject with no mention is still created, so its create race must
// serialise too). The persister locks this whole set up front. Order does not matter — the lock query
// sorts by the hashed key to stay deadlock-free.
func entityNames(in PersistInput) []string {
	seen := make(map[string]struct{}, len(in.Entities)+len(in.Claims))
	names := make([]string, 0, len(in.Entities)+len(in.Claims))
	add := func(name string) {
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	for _, e := range in.Entities {
		add(e.Name)
	}
	for _, c := range in.Claims {
		add(c.Entity)
	}
	return names
}

// acquireEntityLocks serialises the pass's per-entity critical section by taking a transaction-scoped
// advisory lock for every entity it touches, all at once and before any write. It times the acquire and
// logs the wait, warning past advisoryLockWarnThreshold so contention is observable. An empty set is a
// no-op (nothing to serialise), skipped so it costs no round-trip.
func acquireEntityLocks(ctx context.Context, q *db.Queries, runID string, projectID pgtype.UUID, names []string) (time.Duration, error) {
	if len(names) == 0 {
		return 0, nil
	}
	start := time.Now()
	if err := q.AcquireEntityLocks(ctx, db.AcquireEntityLocksParams{ProjectID: projectID, EntityNames: names}); err != nil {
		return 0, fmt.Errorf("acquire entity locks: %w", err)
	}
	waited := time.Since(start)
	attrs := []any{slog.String("run_id", runID), slog.Int("entities", len(names)), slog.Duration("waited", waited)}
	if waited >= advisoryLockWarnThreshold {
		slog.WarnContext(ctx, "extract_run: slow advisory lock acquire", attrs...)
	} else {
		slog.DebugContext(ctx, "extract_run: advisory locks acquired", attrs...)
	}
	return waited, nil
}

// Near-duplicate dedup thresholds and bucket cap. Cosine similarity = 1 - cosine distance for the unit
// vectors the embeddings use, so the distance bands are the complements below. Named constants keep the
// merge policy legible and tunable in one place.
const (
	// nearMergeCosine: at or above this similarity a candidate is a near duplicate and merges (the
	// incoming write supersedes the stored one). 0.92 is the auto-merge threshold.
	nearMergeCosine = 0.92
	// grayZoneCosine: the lower bound of the grey band. A candidate in [grayZoneCosine, nearMergeCosine)
	// is too similar to be confidently distinct but not similar enough to auto-merge; L1 keeps it separate
	// and only records its score. The dual-threshold + adjudication funnel that resolves the grey band is
	// a later increment; review_status is deliberately not touched here.
	grayZoneCosine = 0.85
	// dedupBucketScanCap bounds how many live bucket members the similarity probe compares against, so a
	// large bucket (e.g. the shared empty-entity-context bucket) never runs an unbounded scan.
	dedupBucketScanCap = 200
)

// nearMergeMaxDistance and grayZoneMaxDistance are the cosine-DISTANCE complements of the similarity
// thresholds (distance = 1 - similarity), the form the probe returns and the code compares on.
const (
	nearMergeMaxDistance = 1 - nearMergeCosine
	grayZoneMaxDistance  = 1 - grayZoneCosine
)

// dedupOutcome is how consolidateMemory resolved one candidate, so the caller knows whether to (re)store
// its embedding and how to tally the pass.
type dedupOutcome int

const (
	// outcomeInserted: a fresh row was written (no exact and no near duplicate — or a grey-band neighbour
	// L1 keeps separate). Its embedding must be stored.
	outcomeInserted dedupOutcome = iota
	// outcomeExactMerged: an identical restatement merged into an existing live memory. Content and
	// embedding are unchanged, so neither is rewritten.
	outcomeExactMerged
	// outcomeNearMerged: a near duplicate merged into an existing memory, the incoming content superseding
	// the stored one. The embedding must be re-stored (the content changed).
	outcomeNearMerged
)

// consolidation is the result of consolidating one candidate: the id a same-event claim should link to,
// how it was resolved, and whether a grey-band neighbour was seen (telemetry only). The remaining fields are
// dedup-funnel telemetry for the caller to record after the pass commits: decision is the similarity outcome
// (near_merge, gray_zone, distinct; empty when no similarity candidate was probed), cosine is the best
// candidate's similarity at that decision, and bucketOverflow reports the scan cap was exceeded.
type consolidation struct {
	id             pgtype.UUID
	outcome        dedupOutcome
	grayZone       bool
	decision       string
	cosine         float64
	bucketOverflow bool
}

// consolidateMemory deduplicates one distilled memory against the project's live memories, returning the
// id a same-event claim should link to and how it was resolved. It runs a two-stage funnel, both stages
// inside the pass's transaction under the pass's per-entity advisory lock — so the whole thing is
// serialised per entity bucket and deterministic: "incoming wins" below is arrival order, never a race:
//
//  1. EXACT: an identical restatement (same content_hash) merges into the live row unchanged — version
//     bumped, retained content snapshotted into memory_versions. Content and embedding are untouched.
//  2. NEAR: on an exact miss, the most-similar live memory in the same entity bucket (same context_hash,
//     single model space) is found by embedding cosine distance. At or above nearMergeCosine the incoming
//     memory SUPERSEDES it (last-write-wins, the same policy claims use): the old content is snapshotted
//     into memory_versions and the live content, fingerprint, and provenance become the incoming
//     memory's; the caller re-stores the incoming embedding. In the grey band the candidate is kept
//     separate and only its score is recorded. Otherwise it is a fresh insert.
//
// Blocking to the entity bucket (and the empty-context bucket for entity-less memories) keeps the
// similarity search off an O(N) whole-project scan; a bucket past the scan cap is logged. A lost race on
// an unlocked path leaves a harmless duplicate, never a wrong merge (false-merge ≫ false-miss).
func consolidateMemory(ctx context.Context, q *db.Queries, projectID pgtype.UUID, entityNames []string, m MemoryWrite, vec pgvector.Vector, modelID string) (consolidation, error) {
	contentHash := contentFingerprint(m.Kind, entityNames, m.Content)
	contextHash := contextFingerprint(m.Kind, entityNames)

	// Stage 1 — exact restatement.
	existing, err := q.FindActiveMemoryByContentHash(ctx, db.FindActiveMemoryByContentHashParams{ProjectID: projectID, ContentHash: contentHash})
	switch {
	case err == nil:
		if merr := recordExactMerge(ctx, q, projectID, existing.ID, existing.Content, m); merr != nil {
			return consolidation{}, merr
		}
		return consolidation{id: existing.ID, outcome: outcomeExactMerged}, nil
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to the similarity probe
	default:
		return consolidation{}, fmt.Errorf("exact dedup probe: %w", err)
	}

	// Stage 2 — near duplicate within the entity bucket. decision/cosine/bucketOverflow are dedup-funnel
	// telemetry returned to the caller (recorded after the pass commits); they stay zero when no similarity
	// candidate was probed (an exact merge above, or an empty bucket below).
	var decision string
	var cosine float64
	var bucketOverflow bool
	sim, serr := q.FindNearestLiveMemoryInBucket(ctx, db.FindNearestLiveMemoryInBucketParams{
		QueryVec: vec, ProjectID: projectID, ContextHash: contextHash, ModelID: modelID, ScanCap: dedupBucketScanCap,
	})
	switch {
	case serr == nil:
		bucketOverflow = sim.BucketSize > int64(dedupBucketScanCap)
		if bucketOverflow {
			slog.WarnContext(ctx, "consolidation: similarity bucket exceeded scan cap; members beyond it were not compared",
				slog.Int64("bucket_size", sim.BucketSize), slog.Int("scan_cap", dedupBucketScanCap))
		}
		cosine = 1 - sim.Distance
		switch {
		case sim.Distance <= nearMergeMaxDistance:
			decision = "near_merge"
			if merr := recordNearMerge(ctx, q, projectID, sim.ID, m, entityNames, contentHash, contextHash, cosine); merr != nil {
				return consolidation{}, merr
			}
			slog.InfoContext(ctx, "consolidation: near-duplicate merged, incoming supersedes",
				slog.String("memory_id", uuidString(sim.ID)), slog.Float64("cosine", cosine), slog.Int64("source_seq", m.SourceSeq))
			return consolidation{id: sim.ID, outcome: outcomeNearMerged, decision: decision, cosine: cosine, bucketOverflow: bucketOverflow}, nil
		case sim.Distance <= grayZoneMaxDistance:
			// Grey band: recorded but not merged (L1). The score feeds the L2 threshold-tuning histogram.
			decision = "gray_zone"
			slog.InfoContext(ctx, "consolidation: near-duplicate below merge threshold, kept separate",
				slog.Float64("cosine", cosine), slog.Int64("source_seq", m.SourceSeq))
			id, ierr := insertDistilledMemory(ctx, q, projectID, m, contentHash, contextHash)
			if ierr != nil {
				return consolidation{}, ierr
			}
			return consolidation{id: id, outcome: outcomeInserted, grayZone: true, decision: decision, cosine: cosine, bucketOverflow: bucketOverflow}, nil
		}
		// distance > grayZoneMaxDistance: a candidate existed but was too far to merge — a "distinct" decision.
		decision = "distinct"
	case errors.Is(serr, pgx.ErrNoRows):
		// empty bucket — no candidate probed; fall through to insert with an empty decision.
	default:
		return consolidation{}, fmt.Errorf("similarity dedup probe: %w", serr)
	}

	// Stage 3 — fresh insert.
	id, ierr := insertDistilledMemory(ctx, q, projectID, m, contentHash, contextHash)
	if ierr != nil {
		return consolidation{}, ierr
	}
	return consolidation{id: id, outcome: outcomeInserted, decision: decision, cosine: cosine, bucketOverflow: bucketOverflow}, nil
}

// insertDistilledMemory inserts a fresh memory stamped with both fingerprints so the next pass finds it:
// content_hash for an exact restatement, context_hash for the near-duplicate bucket.
func insertDistilledMemory(ctx context.Context, q *db.Queries, projectID pgtype.UUID, m MemoryWrite, contentHash, contextHash []byte) (pgtype.UUID, error) {
	agent := m.CreatedByAgent
	id, err := q.InsertMemory(ctx, db.InsertMemoryParams{
		ProjectID:      projectID,
		Kind:           m.Kind,
		Content:        m.Content,
		SourceEventID:  m.SourceEventID,
		CreatedByAgent: &agent,
		ContentHash:    contentHash,
		ContextHash:    contextHash,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("insert memory: %w", err)
	}
	return id, nil
}

// recordExactMerge merges an identical restatement into an existing live memory: bump the version and
// snapshot the memory's RETAINED (unchanged) content into memory_versions with the re-observing agent and
// a reason. Content and embedding are unchanged, so neither is rewritten.
func recordExactMerge(ctx context.Context, q *db.Queries, projectID, memoryID pgtype.UUID, retainedContent string, m MemoryWrite) error {
	newVersion, err := q.IncrementMemoryVersion(ctx, db.IncrementMemoryVersionParams{ProjectID: projectID, ID: memoryID})
	if err != nil {
		return fmt.Errorf("bump memory version: %w", err)
	}
	reason := fmt.Sprintf("merged duplicate content from event seq %d", m.SourceSeq)
	return recordSupersededVersion(ctx, q, projectID, memoryID, newVersion, retainedContent, m.CreatedByAgent, reason)
}

// recordNearMerge merges a near duplicate where the incoming memory SUPERSEDES the stored one. It
// overwrites the live content, exact fingerprint, and provenance with the incoming memory's (version
// bumped) and snapshots the content that was live just before — read UNDER the UPDATE's row lock, not the
// pre-lock similarity read — into memory_versions with a reason naming the cosine score and both content
// fingerprints, so the superseded phrasing stays queryable even under a concurrent near-merge of the same
// memory. The caller re-stores the incoming embedding under the same id (the content changed).
func recordNearMerge(ctx context.Context, q *db.Queries, projectID, memoryID pgtype.UUID, m MemoryWrite, entityNames []string, newContentHash, contextHash []byte, cosine float64) error {
	changedBy := m.CreatedByAgent
	// Lock the target row and read the content that is live right now, so the snapshot below reflects the
	// true prior content even if a concurrent near-merge of the same memory committed first (this blocks
	// until it does, then reads its committed content).
	priorContent, err := q.ReadMemoryContentForUpdate(ctx, db.ReadMemoryContentForUpdateParams{ProjectID: projectID, ID: memoryID})
	if err != nil {
		return fmt.Errorf("lock memory for near-merge: %w", err)
	}
	newVersion, err := q.UpdateMemoryOnNearMerge(ctx, db.UpdateMemoryOnNearMergeParams{
		ProjectID:      projectID,
		ID:             memoryID,
		Content:        m.Content,
		ContentHash:    newContentHash,
		ContextHash:    contextHash,
		SourceEventID:  m.SourceEventID,
		CreatedByAgent: &changedBy,
	})
	if err != nil {
		return fmt.Errorf("near-merge update: %w", err)
	}
	oldContentHash := contentFingerprint(m.Kind, entityNames, priorContent)
	reason := fmt.Sprintf("near-duplicate (cosine %.4f) superseded by event seq %d; %x -> %x",
		cosine, m.SourceSeq, fingerprintPrefix(oldContentHash), fingerprintPrefix(newContentHash))
	return recordSupersededVersion(ctx, q, projectID, memoryID, newVersion, priorContent, changedBy, reason)
}

// recordSupersededVersion snapshots into memory_versions the content and reason of the version a merge
// just retired. Convention: memory_versions[K] holds the content that was live at version K and the reason
// it was superseded by K+1 — a tombstone for the K→K+1 transition, mirroring claims.resolution_reason.
// newVersion is the version the merge produced, so the snapshot is the prior version newVersion-1; the
// live row is always the highest version, and the first supersede creates the first history row (a memory
// at version 1 with no merges has no history). Both merge paths go through here, so the convention cannot
// diverge between them.
func recordSupersededVersion(ctx context.Context, q *db.Queries, projectID, memoryID pgtype.UUID, newVersion int32, retiredContent, changedBy, reason string) error {
	if _, err := q.InsertMemoryVersion(ctx, db.InsertMemoryVersionParams{
		ProjectID: projectID,
		MemoryID:  memoryID,
		Version:   newVersion - 1,
		Content:   retiredContent,
		ChangedBy: &changedBy,
		Reason:    &reason,
	}); err != nil {
		return fmt.Errorf("record superseded version: %w", err)
	}
	return nil
}

// fingerprintPrefix is the leading bytes of a content fingerprint, enough to identify it in an audit
// reason without printing the whole hash.
func fingerprintPrefix(h []byte) []byte {
	const n = 8
	if len(h) < n {
		return h
	}
	return h[:n]
}
