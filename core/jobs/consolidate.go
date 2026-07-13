package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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

// contentFingerprint is the dedup key for a memory: the SHA-256 over its normalization version, its
// kind, its entity context (the sorted set of entity names the pass touches), and its normalized
// content. Folding kind and the entity context in — not just the text — keeps dedup INSIDE an entity
// bucket: identical text in a different entity context (or bound to different entities by a variant
// re-extraction) yields a different fingerprint and stays a separate memory, rather than being silently
// merged across contexts. Entity-less memories share an empty context, so identical entity-less content
// still merges (the correct behavior). Every field is length-prefixed and the entity count is written
// before the names, so the preimage is unambiguous: no two distinct (kind, entities, content) triples
// can collide by field-boundary aliasing. Exact fingerprint equality means the inputs are identical
// after normalization — never mere similarity — so it cannot cause a near-duplicate false merge.
func contentFingerprint(kind string, entityNames []string, content string) []byte {
	names := append([]string(nil), entityNames...)
	sort.Strings(names)

	h := sha256.New()
	writeField := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	writeField(normContentVersion)
	writeField(kind)
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(names)))
	h.Write(count[:])
	for _, n := range names {
		writeField(n)
	}
	writeField(normalizeContent(content))
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
func acquireEntityLocks(ctx context.Context, q *db.Queries, runID string, projectID pgtype.UUID, names []string) error {
	if len(names) == 0 {
		return nil
	}
	start := time.Now()
	if err := q.AcquireEntityLocks(ctx, db.AcquireEntityLocksParams{ProjectID: projectID, EntityNames: names}); err != nil {
		return fmt.Errorf("acquire entity locks: %w", err)
	}
	waited := time.Since(start)
	attrs := []any{slog.String("run_id", runID), slog.Int("entities", len(names)), slog.Duration("waited", waited)}
	if waited >= advisoryLockWarnThreshold {
		slog.WarnContext(ctx, "extract_run: slow advisory lock acquire", attrs...)
	} else {
		slog.DebugContext(ctx, "extract_run: advisory locks acquired", attrs...)
	}
	return nil
}

// consolidateMemory deduplicates one distilled memory against the project's live memories by exact
// normalized content WITHIN its entity context (entityNames is the pass's sorted entity set, folded into
// the fingerprint), returning the id of the memory the caller should link same-event claims to. On a hit
// it MERGES: the existing memory absorbs the restatement — its version is bumped and the merge is
// recorded in memory_versions (with the re-observing agent and a reason) — and no new row is inserted, so
// merged is true and the returned id is the existing memory's. On a miss it inserts a fresh row carrying
// the fingerprint and returns its id with merged false. The lookup and the insert/merge both run inside
// the pass's transaction under the entity lock, so the common (entity-associated) path is race-free; a
// lost race on an unlocked path leaves a duplicate, which is the acceptable direction (a miss, never a
// wrong merge).
func consolidateMemory(ctx context.Context, q *db.Queries, projectID pgtype.UUID, entityNames []string, m MemoryWrite) (id pgtype.UUID, merged bool, err error) {
	fp := contentFingerprint(m.Kind, entityNames, m.Content)

	existing, err := q.FindActiveMemoryByContentHash(ctx, db.FindActiveMemoryByContentHashParams{ProjectID: projectID, ContentHash: fp})
	switch {
	case err == nil:
		// Duplicate restatement: merge into the existing memory. Bump its version and record the merge in
		// memory_versions, snapshotting the memory's RETAINED content (exact dedup keeps the first-observed
		// form, so the version row stays consistent with the live row); the reason and re-observing agent
		// capture who restated it. A later differing-content merge writes the resulting content here instead.
		newVersion, verr := q.IncrementMemoryVersion(ctx, db.IncrementMemoryVersionParams{ProjectID: projectID, ID: existing.ID})
		if verr != nil {
			return pgtype.UUID{}, false, fmt.Errorf("bump memory version: %w", verr)
		}
		changedBy := m.CreatedByAgent
		reason := fmt.Sprintf("merged duplicate content from event seq %d", m.SourceSeq)
		if _, verr := q.InsertMemoryVersion(ctx, db.InsertMemoryVersionParams{
			ProjectID: projectID,
			MemoryID:  existing.ID,
			Version:   newVersion,
			Content:   existing.Content,
			ChangedBy: &changedBy,
			Reason:    &reason,
		}); verr != nil {
			return pgtype.UUID{}, false, fmt.Errorf("record merged version: %w", verr)
		}
		return existing.ID, true, nil

	case errors.Is(err, pgx.ErrNoRows):
		// No live duplicate: insert fresh, stamped with its fingerprint so the next restatement finds it.
		agent := m.CreatedByAgent
		id, err = q.InsertMemory(ctx, db.InsertMemoryParams{
			ProjectID:      projectID,
			Kind:           m.Kind,
			Content:        m.Content,
			SourceEventID:  m.SourceEventID,
			CreatedByAgent: &agent,
			ContentHash:    fp,
		})
		if err != nil {
			return pgtype.UUID{}, false, fmt.Errorf("insert memory: %w", err)
		}
		return id, false, nil

	default:
		return pgtype.UUID{}, false, fmt.Errorf("dedup probe: %w", err)
	}
}
