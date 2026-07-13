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

	"github.com/lore-gpt/lore/core/ext"
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
	store       tenantRunner
	adjudicator ext.Adjudicator
}

// NewPGPersister builds the OSS persister over a tenant-scoped transaction runner (the store) and the
// conflict-resolution policy. A downstream build injects a different Adjudicator without forking the persister.
func NewPGPersister(store tenantRunner, adjudicator ext.Adjudicator) *PGPersister {
	return &PGPersister{store: store, adjudicator: adjudicator}
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
func (p *PGPersister) Persist(ctx context.Context, in PersistInput) error {
	return p.store.WithProject(ctx, in.ProjectID, func(tx pgx.Tx) error {
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

		// Deduplicate then persist memories, indexing the first memory per source event so a same-event
		// claim can link to it (a claim from an event with no memory is stored standalone, memory_id
		// NULL). A duplicate restatement merges into the existing memory instead of inserting a new row.
		memoryBySeq := make(map[int64]pgtype.UUID, len(in.Memories))
		var inserted, mergedCount int
		for _, m := range in.Memories {
			id, merged, err := consolidateMemory(ctx, q, in.ProjectID, names, m)
			if err != nil {
				return err
			}
			if merged {
				mergedCount++
			} else {
				inserted++
			}
			if _, ok := memoryBySeq[m.SourceSeq]; !ok {
				memoryBySeq[m.SourceSeq] = id
			}
		}

		// Claims in SourceSeq order: resolve the subject, and if an active claim already asserts it, run
		// the conflict through the Adjudicator (default last-write-wins). The resolved value becomes the
		// new active claim; the old one is superseded and stamped with the policy's reason. Pre-generating
		// the id lets the supersede point at this replacement before it exists (the self-FK is deferred,
		// validated at commit).
		for _, c := range in.Claims {
			entityID, ok := entityIDs[c.Entity]
			if !ok {
				// A claim subject not among the mentions: register it with an unknown type.
				id, err := upsertEntity(ctx, q, in.ProjectID, c.Entity, "unknown", nil)
				if err != nil {
					return err
				}
				entityID = id
				entityIDs[c.Entity] = id
			}

			claimID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
			value := c.Value

			active, err := q.GetActiveClaimBySubject(ctx, db.GetActiveClaimBySubjectParams{
				ProjectID: in.ProjectID,
				EntityID:  entityID,
				Predicate: c.Predicate,
			})
			switch {
			case err == nil:
				// A conflict: resolve it, store the resolved value, and supersede the old claim with the
				// policy's reason. Provenance rides along for the reason only — never to order the two.
				res, aerr := p.adjudicator.Resolve(ctx, ext.Conflict{
					ProjectID:      uuidString(in.ProjectID),
					Current:        active.Value,
					Incoming:       c.Value,
					CurrentSource:  ext.Provenance{RunID: uuidString(active.RunID), Seq: derefSeq(active.Seq)},
					IncomingSource: ext.Provenance{RunID: uuidString(in.RunID), Seq: c.SourceSeq},
				})
				if aerr != nil {
					return fmt.Errorf("adjudicate claim conflict: %w", aerr)
				}
				value = res.Value
				reason := conflictReason(res.Reason, claimID, in.RunID, c.SourceSeq, active.ID, active.RunID, active.Seq)
				if _, err := q.SupersedeActiveClaimBySubject(ctx, db.SupersedeActiveClaimBySubjectParams{
					SupersededBy:     claimID,
					ResolutionReason: &reason,
					ProjectID:        in.ProjectID,
					EntityID:         entityID,
					Predicate:        c.Predicate,
				}); err != nil {
					return fmt.Errorf("supersede active claim: %w", err)
				}
			case errors.Is(err, pgx.ErrNoRows):
				// First assertion of this subject: nothing to supersede or adjudicate.
			default:
				return fmt.Errorf("read active claim: %w", err)
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
			slog.Int("memories_merged", mergedCount),
			slog.Int("claims", len(in.Claims)),
			slog.Int("entities", len(in.Entities)))
		return nil
	})
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
