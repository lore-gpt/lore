package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/store/db"
)

// uuidString renders a pgtype.UUID as its canonical string for logging. An invalid (zero) UUID renders
// as the nil UUID, which is fine for a log line.
func uuidString(u pgtype.UUID) string {
	return uuid.UUID(u.Bytes).String()
}

// derefSeq returns the provenance seq, or 0 when it is absent (a claim with no source event). seq is
// 1-based, so 0 unambiguously reads as "no source".
func derefSeq(seq *int64) int64 {
	if seq == nil {
		return 0
	}
	return *seq
}

// conflictReason is the audit line stamped on a superseded claim: the policy that decided, and the
// winning (incoming) and losing (superseded) claims with their run/seq provenance.
func conflictReason(policy string, winnerID, winnerRun pgtype.UUID, winnerSeq int64, loserID, loserRun pgtype.UUID, loserSeq *int64) string {
	return fmt.Sprintf("%s: claim %s (run %s seq %d) supersedes claim %s (run %s seq %d)",
		policy, uuidString(winnerID), uuidString(winnerRun), winnerSeq,
		uuidString(loserID), uuidString(loserRun), derefSeq(loserSeq))
}

// claimSubject is the natural key of a claim — its (entity, predicate) within a project. The persister
// keys its in-pass overlay of active claims by it.
type claimSubject struct {
	entityID  pgtype.UUID
	predicate string
}

// activeClaim is the currently-active claim for a subject as the persister sees it mid-pass: either the
// row read from the database at the start of the pass, or one this pass just inserted. Its value and
// run/seq provenance feed conflict resolution and the recorded reason exactly as a per-claim re-read
// would.
type activeClaim struct {
	id    pgtype.UUID
	value []byte
	runID pgtype.UUID
	seq   *int64
}

// ErrModelMismatch means the project's active embedding model does not match the composed embedder, so a
// pass would write vectors in a second model's space. The persister fails the pass loudly rather than
// corrupt the partition's single-model invariant (the read path has its own mismatch guard). It surfaces
// through the extraction job's failure — a retry keeps failing until the deployment's embedder is restored
// (or the model deliberately migrated), which is the intended loud signal, not silent corruption.
var ErrModelMismatch = errors.New("persist: project active model does not match the embedder")

// Persister commits one extraction pass: it writes the distilled memories, entities, and claims and
// advances the run's checkpoint, all in a single transaction. That atomicity is what makes a
// coalesced pass idempotent — the checkpoint only moves past the events a pass consumed if the rows
// for those events committed, so a retried or re-coalesced pass never double-writes and a crashed one
// is reprocessed cleanly. PGPersister is the OSS implementation; tests supply a fake.
type Persister interface {
	Persist(ctx context.Context, in PersistInput) error
	// SetRunBatch records the handle and covered seq of a just-submitted economy-mode batch on the
	// run, so a later job attempt can collect it. It is a separate tenant-scoped write from Persist
	// because a batch's submit and collect happen in different attempts; AdvanceCoveredSeq clears the
	// recorded state when the collected pass commits.
	SetRunBatch(ctx context.Context, projectID, runID pgtype.UUID, handle string, coveredSeq int64) error
}

// PersistInput is one committed unit of extraction for a run: the distilled memories, the entities
// they mention, and the structured claims — plus the checkpoint move. CoveredSeq is the highest seq the
// pass READ (gated events included), so archived machine chatter is marked consumed and never re-read.
// ExpectedCoveredSeq is the checkpoint value the pass started from; the persister advances the checkpoint
// with a compare-and-swap on it, so a concurrent pass that already advanced this run makes the advance a
// no-op and this pass rolls back rather than double-writing. Claims must be ordered by SourceSeq so
// subject conflicts resolve last-write-wins.
type PersistInput struct {
	ProjectID          pgtype.UUID
	RunID              pgtype.UUID
	ExpectedCoveredSeq int64
	CoveredSeq         int64
	Memories           []MemoryWrite
	Entities           []EntityWrite
	Claims             []ClaimWrite
}

// MemoryWrite is one memory ready to store, with provenance already resolved from the source event:
// SourceEventID is the event it was distilled from and CreatedByAgent is that event's agent. SourceSeq
// links it back to that event so a claim from the same event can point at this memory.
type MemoryWrite struct {
	Kind           string
	Content        string
	SourceEventID  pgtype.UUID
	CreatedByAgent string
	SourceSeq      int64
}

// EntityWrite is one entity mention to register (get-or-create by name within the project).
type EntityWrite struct {
	Name    string
	Type    string
	Aliases []string
}

// ClaimWrite is one structured assertion to store. Entity is the subject's name (resolved to an id at
// persist time); SourceEventID carries provenance; SourceSeq both links the claim to a same-event
// memory and orders last-write-wins supersession.
type ClaimWrite struct {
	Entity        string
	Predicate     string
	Value         json.RawMessage
	EventTime     *time.Time
	SourceEventID pgtype.UUID
	SourceSeq     int64
}

// tenantRunner runs fn inside a transaction scoped to projectID (RLS lore.project_id set). *store.Store
// satisfies it; naming the one method the persister needs keeps jobs off the whole store surface and
// makes the persister unit-testable without a database.
type tenantRunner interface {
	WithProject(ctx context.Context, projectID pgtype.UUID, fn func(pgx.Tx) error) error
}

// PGPersister persists an extraction pass to Postgres inside a tenant-scoped transaction. It resolves
// claim conflicts through the injected Adjudicator (the OSS default is last-write-wins).
type PGPersister struct {
	store         tenantRunner
	adjudicator   ext.Adjudicator
	embedder      ext.Embedder
	indexEnqueuer IndexEnqueuer
	metrics       *metrics.Registry
}

// PersisterOption configures an optional dependency of the persister.
type PersisterOption func(*PGPersister)

// WithIndexEnqueuer wires the enqueuer that schedules a one-time vector-index build when a project first
// pins its embedding model. Without it the persister still pins the model; the index build is simply not
// enqueued from the write path (the worker's startup sweep reconciles it), so unit tests need no queue.
func WithIndexEnqueuer(e IndexEnqueuer) PersisterOption {
	return func(p *PGPersister) { p.indexEnqueuer = e }
}

// WithPersisterMetrics wires the Prometheus instrument set; a nil registry is ignored (the no-op default
// stays), so instrumentation runs unconditionally.
func WithPersisterMetrics(m *metrics.Registry) PersisterOption {
	return func(p *PGPersister) {
		if m != nil {
			p.metrics = m
		}
	}
}

// NewPGPersister builds the OSS persister over a tenant-scoped transaction runner (the store), the
// conflict-resolution policy, and the embedding provider. A downstream build injects a different
// Adjudicator or Embedder without forking the persister.
func NewPGPersister(store tenantRunner, adjudicator ext.Adjudicator, embedder ext.Embedder, opts ...PersisterOption) *PGPersister {
	p := &PGPersister{store: store, adjudicator: adjudicator, embedder: embedder, metrics: metrics.NewNoop()}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Persist writes the pass's entities, memories, and claims and advances the run checkpoint in one
// transaction. It first serialises the per-entity critical section by locking every entity the pass
// touches, so a concurrent pass over a shared entity waits rather than racing. Entities are registered
// so claims resolve their subjects; each memory is deduplicated against the project's live memories
// (identical content merges into the existing row rather than inserting a duplicate) and indexed by
// source seq so a same-event claim links to it; claims apply last-write-wins per subject (supersede the
// active claim, then insert). The checkpoint advance is a compare-and-swap on the value the pass started
// from, so a concurrent double-delivery leaves exactly one set of rows and one advance: the loser's
// advance matches no row and it rolls back with errCheckpointConflict.
//
// Candidate contents are embedded BEFORE the transaction opens, so a real, network-backed embedder's
// latency never holds the entity locks (or the transaction) open; a newly inserted memory's vector is
// then stored inside the transaction, keyed by content.
func (p *PGPersister) Persist(ctx context.Context, in PersistInput) error {
	// Embed candidate contents up front, outside the transaction and its entity locks.
	memoryVectors, err := p.embedContents(ctx, in.Memories)
	if err != nil {
		return err
	}

	// Consolidation outcome counts, declared outside the transaction so they can be recorded to metrics only
	// after the transaction commits (the closure accumulates them; a commit-phase failure discards them).
	var inserted, exactMerged, nearMerged, grayZone int
	err = p.store.WithProject(ctx, in.ProjectID, func(tx pgx.Tx) error {
		q := db.New(tx)

		// The entity context of this pass: every entity it touches (mentions and claim subjects). It both
		// serialises the critical section (the lock set) and scopes dedup (folded into each memory's
		// fingerprint), so identical text in a different entity context is not merged.
		names := entityNames(in)

		// Serialise the whole per-entity critical section before any write: lock every entity this pass
		// touches, all at once, deadlock-free.
		if err := acquireEntityLocks(ctx, q, uuidString(in.RunID), in.ProjectID, names); err != nil {
			return err
		}

		// Register mentioned entities up front so their names resolve to ids for the claims below.
		entityIDs := make(map[string]pgtype.UUID, len(in.Entities))
		for _, e := range in.Entities {
			id, err := upsertEntity(ctx, q, in.ProjectID, e.Name, e.Type, e.Aliases)
			if err != nil {
				return err
			}
			entityIDs[e.Name] = id
		}

		// Pin the project's active embedding model on the first pass that has vectors to write, and reject a
		// mismatch before storing any. The pin is first-wins (coalesce), so a fresh project adopts the running
		// embedder and a concurrent first pass leaves exactly one winner; the returned effective model then
		// gates the write — a project already pinned to a DIFFERENT model than the running embedder must never
		// get vectors in a second model's space, so the pass fails loudly instead. Only passes that actually
		// embed something reach here (no memories → no pin, no model chosen prematurely).
		if len(memoryVectors) > 0 {
			modelID := p.embedder.ModelID()
			pinned, err := q.PinActiveModelIfUnset(ctx, db.PinActiveModelIfUnsetParams{
				ProjectID: in.ProjectID,
				ModelID:   &modelID,
			})
			if err != nil {
				return fmt.Errorf("pin active model: %w", err)
			}
			if pinned == 1 {
				// This pass chose the model (first-wins, exactly one racer gets it). Enqueue a one-time index
				// build for the newly pinned partition IN THIS transaction, so it commits atomically with the
				// pin and never fires for a rolled-back pass. The index is a performance optimisation — recall
				// works on the exact-scan path until it exists — so a nil enqueuer (no worker wired) just skips
				// it, and the worker's startup sweep reconciles anything missed.
				if p.indexEnqueuer != nil {
					if err := p.indexEnqueuer.EnqueueEnsureIndex(ctx, tx, in.ProjectID); err != nil {
						return fmt.Errorf("enqueue index build: %w", err)
					}
				}
			} else {
				// Already pinned (a prior pass, or a concurrent winner). Read the effective model in a fresh
				// statement — which sees a concurrent winner's commit — and reject a mismatch: a project pinned
				// to a DIFFERENT model than the running embedder must never get vectors in a second model's space.
				effective, err := q.GetActiveModelID(ctx, in.ProjectID)
				if err != nil {
					return fmt.Errorf("read active model: %w", err)
				}
				got := ""
				if effective != nil {
					got = *effective
				}
				if got != modelID {
					return fmt.Errorf("%w: project active model %q, embedder %q", ErrModelMismatch, got, modelID)
				}
			}
		}

		// Deduplicate then persist memories, indexing the first memory per source event so a same-event
		// claim can link to it (a claim from an event with no memory is stored standalone, memory_id
		// NULL). consolidateMemory resolves each candidate to a fresh insert, an exact-restatement merge,
		// or a near-duplicate merge (the incoming content supersedes the stored one). A fresh insert and a
		// near-merge both store the incoming vector — a near-merge overwrote the content, so its vector
		// changed; an exact restatement leaves content and vector unchanged, so its embedding is not
		// rewritten.
		memoryBySeq := make(map[int64]pgtype.UUID, len(in.Memories))
		for _, m := range in.Memories {
			res, err := consolidateMemory(ctx, q, in.ProjectID, names, m, memoryVectors[m.Content], p.embedder.ModelID())
			if err != nil {
				return err
			}
			switch res.outcome {
			case outcomeExactMerged:
				exactMerged++
			case outcomeNearMerged:
				nearMerged++
			default:
				inserted++
				if res.grayZone {
					grayZone++
				}
			}
			if res.outcome != outcomeExactMerged {
				if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
					ProjectID: in.ProjectID,
					MemoryID:  res.id,
					ModelID:   p.embedder.ModelID(),
					Vec:       memoryVectors[m.Content],
				}); err != nil {
					return fmt.Errorf("upsert embedding for memory %s: %w", uuidString(res.id), err)
				}
			}
			if _, ok := memoryBySeq[m.SourceSeq]; !ok {
				memoryBySeq[m.SourceSeq] = res.id
			}
		}

		// Resolve every claim subject to an entity id up front (registering any not among the mentions with
		// an unknown type), so the active claims for all of them can be read in a single round-trip below
		// rather than once per claim.
		claimEntityIDs := make([]pgtype.UUID, 0, len(in.Claims))
		seenClaimEntity := make(map[pgtype.UUID]struct{}, len(in.Claims))
		for _, c := range in.Claims {
			entityID, ok := entityIDs[c.Entity]
			if !ok {
				id, err := upsertEntity(ctx, q, in.ProjectID, c.Entity, "unknown", nil)
				if err != nil {
					return err
				}
				entityID = id
				entityIDs[c.Entity] = id
			}
			if _, seen := seenClaimEntity[entityID]; !seen {
				seenClaimEntity[entityID] = struct{}{}
				claimEntityIDs = append(claimEntityIDs, entityID)
			}
		}

		// Read the active claims for those entities once and key them by full subject. This overlay stands
		// in for the per-claim read: the resolution loop reads and updates it in memory, so two claims for
		// the same subject in one pass still resolve last-write-wins. When the loop re-read per claim, a
		// transaction saw its own just-inserted claim; the overlay reproduces exactly that (it records each
		// inserted claim as the subject's active one) without the extra round-trips.
		overlay := make(map[claimSubject]activeClaim, len(claimEntityIDs))
		if len(claimEntityIDs) > 0 {
			rows, err := q.GetActiveClaimsByEntities(ctx, db.GetActiveClaimsByEntitiesParams{
				ProjectID: in.ProjectID,
				EntityIds: claimEntityIDs,
			})
			if err != nil {
				return fmt.Errorf("read active claims: %w", err)
			}
			for _, r := range rows {
				overlay[claimSubject{entityID: r.EntityID, predicate: r.Predicate}] = activeClaim{
					id:    r.ID,
					value: r.Value,
					runID: r.RunID,
					seq:   r.Seq,
				}
			}
		}

		// Claims in SourceSeq order: if the subject already has an active claim (in the overlay), run the
		// conflict through the Adjudicator (default last-write-wins). The resolved value becomes the new
		// active claim; the old one is superseded and stamped with the policy's reason. Pre-generating the id
		// lets the supersede point at this replacement before it exists (the self-FK is deferred, validated
		// at commit).
		for _, c := range in.Claims {
			entityID := entityIDs[c.Entity] // resolved in the pre-pass above
			subject := claimSubject{entityID: entityID, predicate: c.Predicate}
			claimID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
			value := c.Value

			if active, ok := overlay[subject]; ok {
				// A conflict: resolve it, store the resolved value, and supersede the old claim with the
				// policy's reason. Provenance rides along for the reason only — never to order the two.
				res, aerr := p.adjudicator.Resolve(ctx, ext.Conflict{
					ProjectID:      uuidString(in.ProjectID),
					Current:        active.value,
					Incoming:       c.Value,
					CurrentSource:  ext.Provenance{RunID: uuidString(active.runID), Seq: derefSeq(active.seq)},
					IncomingSource: ext.Provenance{RunID: uuidString(in.RunID), Seq: c.SourceSeq},
				})
				if aerr != nil {
					return fmt.Errorf("adjudicate claim conflict: %w", aerr)
				}
				value = res.Value
				reason := conflictReason(res.Reason, claimID, in.RunID, c.SourceSeq, active.id, active.runID, active.seq)
				if _, err := q.SupersedeActiveClaimBySubject(ctx, db.SupersedeActiveClaimBySubjectParams{
					SupersededBy:     claimID,
					ResolutionReason: &reason,
					ProjectID:        in.ProjectID,
					EntityID:         entityID,
					Predicate:        c.Predicate,
				}); err != nil {
					return fmt.Errorf("supersede active claim: %w", err)
				}
			}

			var eventTime pgtype.Timestamptz
			if c.EventTime != nil {
				eventTime = pgtype.Timestamptz{Time: *c.EventTime, Valid: true}
			}
			if err := q.InsertClaim(ctx, db.InsertClaimParams{
				ID:            claimID,
				MemoryID:      memoryBySeq[c.SourceSeq], // zero value (Valid:false) => NULL for a standalone claim
				ProjectID:     in.ProjectID,
				EntityID:      entityID,
				Predicate:     c.Predicate,
				Value:         value,
				EventTime:     eventTime,
				SourceEventID: c.SourceEventID,
			}); err != nil {
				return fmt.Errorf("insert claim: %w", err)
			}

			// Record the just-written claim as this subject's active one, so a later claim for the same
			// subject in this pass supersedes it (last-write-wins) instead of inserting a second active row —
			// the outcome the old per-claim re-read produced through the transaction's own-write visibility.
			// Its provenance mirrors the events join a re-read would do: a claim always carries the event it
			// was distilled from (seq == SourceSeq, in this run), so run/seq are the pass's run and SourceSeq;
			// a claim with no source event has neither, exactly as the left join would yield NULLs.
			next := activeClaim{id: claimID, value: value}
			if c.SourceEventID.Valid {
				seq := c.SourceSeq
				next.runID = in.RunID
				next.seq = &seq
			}
			overlay[subject] = next
		}

		// Advance the checkpoint with a compare-and-swap on the value the pass started from. A match moves
		// it forward and commits everything above with it; a mismatch (another pass advanced this run
		// first) touches no row, so this pass rolls back its writes and reports the conflict.
		rows, err := q.AdvanceCoveredSeq(ctx, db.AdvanceCoveredSeqParams{
			NewCoveredSeq:      in.CoveredSeq,
			RunID:              in.RunID,
			ProjectID:          in.ProjectID,
			ExpectedCoveredSeq: in.ExpectedCoveredSeq,
		})
		if err != nil {
			return fmt.Errorf("advance checkpoint: %w", err)
		}
		if rows == 0 {
			return errCheckpointConflict
		}

		slog.InfoContext(ctx, "consolidation pass persisted",
			slog.String("run_id", uuidString(in.RunID)),
			slog.Int("memories_inserted", inserted),
			slog.Int("memories_exact_merged", exactMerged),
			slog.Int("memories_near_merged", nearMerged),
			slog.Int("memories_gray_zone", grayZone),
			slog.Int("claims", len(in.Claims)),
			slog.Int("entities", len(in.Entities)))
		return nil
	})
	p.recordPassOutcome(err)
	if err == nil {
		// Record the per-memory outcomes only after the commit succeeds, so the counters reflect the committed
		// state (a commit-phase failure that rolled the rows back must not count them, and a River retry that
		// re-runs the pass must not double-count).
		p.metrics.ConsolidationMemories.WithLabelValues("inserted").Add(float64(inserted))
		p.metrics.ConsolidationMemories.WithLabelValues("exact_merged").Add(float64(exactMerged))
		p.metrics.ConsolidationMemories.WithLabelValues("near_merged").Add(float64(nearMerged))
		p.metrics.ConsolidationMemories.WithLabelValues("gray_zone").Add(float64(grayZone))
	}
	return err
}

// recordPassOutcome records the consolidation pass result after the transaction resolves, so the outcome
// reflects the committed state (not a pre-commit optimism). A checkpoint conflict and a write-side model
// mismatch each get their own counter as well as folding into the pass result.
func (p *PGPersister) recordPassOutcome(err error) {
	switch {
	case err == nil:
		p.metrics.ConsolidationPass.WithLabelValues("committed").Inc()
	case errors.Is(err, errCheckpointConflict):
		p.metrics.ConsolidationCheckpointCnf.Inc()
		p.metrics.ConsolidationPass.WithLabelValues("checkpoint_conflict").Inc()
	case errors.Is(err, ErrModelMismatch):
		p.metrics.ConsolidationModelMismatch.Inc()
		p.metrics.ConsolidationPass.WithLabelValues("error").Inc()
	default:
		p.metrics.ConsolidationPass.WithLabelValues("error").Inc()
	}
}

// SetRunBatch records a submitted batch's handle and covered seq on the run inside a tenant-scoped
// transaction, so a later attempt can collect it. A zero-row update means the run is not visible in
// the project (deleted, or a scoping error) — surfaced as an error rather than silently orphaning the
// submitted batch.
func (p *PGPersister) SetRunBatch(ctx context.Context, projectID, runID pgtype.UUID, handle string, coveredSeq int64) error {
	return p.store.WithProject(ctx, projectID, func(tx pgx.Tx) error {
		rows, err := db.New(tx).SetRunBatch(ctx, db.SetRunBatchParams{
			BatchID:         &handle,
			BatchCoveredSeq: &coveredSeq,
			RunID:           runID,
			ProjectID:       projectID,
		})
		if err != nil {
			return fmt.Errorf("set run batch: %w", err)
		}
		if rows == 0 {
			return fmt.Errorf("set run batch: run not found in project")
		}
		return nil
	})
}

// upsertEntity get-or-creates an entity by (project, name), returning its id. A nil aliases slice is
// normalised to an empty array so it satisfies the NOT NULL aliases column.
func upsertEntity(ctx context.Context, q *db.Queries, projectID pgtype.UUID, name, typ string, aliases []string) (pgtype.UUID, error) {
	if aliases == nil {
		aliases = []string{}
	}
	id, err := q.UpsertEntity(ctx, db.UpsertEntityParams{
		ProjectID: projectID,
		Name:      name,
		Type:      typ,
		Aliases:   aliases,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("upsert entity %q: %w", name, err)
	}
	return id, nil
}

// embedContents embeds the distinct contents of the pass's candidate memories, returning a
// content→vector map. It runs BEFORE the consolidation transaction (and its entity locks), so a real,
// network-backed embedder's latency never holds the critical section open. It embeds every distinct
// candidate content in one call — batch-shaped, so a real provider makes a single request — and asserts
// every vector has the embedder's dimension, so a wrong-dimension vector (which the dimensionless column
// would accept, only to fail later at index build) is rejected before any write. Only contents that turn
// out to be new inserts are stored by the caller; a content that merges into an existing memory is
// embedded here but its vector goes unused. An empty set is a no-op.
func (p *PGPersister) embedContents(ctx context.Context, memories []MemoryWrite) (map[string]pgvector.Vector, error) {
	seen := make(map[string]struct{}, len(memories))
	texts := make([]string, 0, len(memories))
	for _, m := range memories {
		if _, ok := seen[m.Content]; ok {
			continue
		}
		seen[m.Content] = struct{}{}
		texts = append(texts, m.Content)
	}
	if len(texts) == 0 {
		return nil, nil
	}
	vecs, err := p.embedder.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("embed memories: %w", err)
	}
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("embed memories: got %d vectors for %d contents", len(vecs), len(texts))
	}
	dim := p.embedder.Dim()
	modelID := p.embedder.ModelID()
	out := make(map[string]pgvector.Vector, len(texts))
	for i, vec := range vecs {
		if len(vec) != dim {
			return nil, fmt.Errorf("embed memories: vector for content %d has length %d, want %d (model %q)",
				i, len(vec), dim, modelID)
		}
		out[texts[i]] = pgvector.NewVector(vec)
	}
	return out, nil
}
