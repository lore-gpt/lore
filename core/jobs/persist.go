package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/store/db"
)

// Persister commits one extraction pass: it writes the distilled memories, entities, and claims and
// advances the run's checkpoint, all in a single transaction. That atomicity is what makes a
// coalesced pass idempotent — the checkpoint only moves past the events a pass consumed if the rows
// for those events committed, so a retried or re-coalesced pass never double-writes and a crashed one
// is reprocessed cleanly. PGPersister is the OSS implementation; tests supply a fake.
type Persister interface {
	Persist(ctx context.Context, in PersistInput) error
}

// PersistInput is one committed unit of extraction for a run: the distilled memories, the entities
// they mention, and the structured claims — plus the checkpoint to advance to. CoveredSeq is the
// highest seq the pass READ (gated events included), so archived machine chatter is marked consumed
// and never re-read. Claims must be ordered by SourceSeq so subject conflicts resolve last-write-wins.
type PersistInput struct {
	ProjectID  pgtype.UUID
	RunID      pgtype.UUID
	CoveredSeq int64
	Memories   []MemoryWrite
	Entities   []EntityWrite
	Claims     []ClaimWrite
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

// PGPersister persists an extraction pass to Postgres inside a tenant-scoped transaction.
type PGPersister struct {
	store tenantRunner
}

// NewPGPersister builds the OSS persister over a tenant-scoped transaction runner (the store).
func NewPGPersister(store tenantRunner) *PGPersister {
	return &PGPersister{store: store}
}

// Persist writes the pass's entities, memories, and claims and advances the run checkpoint in one
// transaction. Entities are registered first so claims resolve their subjects; memories are indexed by
// source seq so a claim from the same event links to its memory; claims apply last-write-wins per
// subject (supersede the active claim, then insert). The checkpoint advance is guarded to only move
// forward, so a duplicate pass is a no-op rather than a regression.
func (p *PGPersister) Persist(ctx context.Context, in PersistInput) error {
	return p.store.WithProject(ctx, in.ProjectID, func(tx pgx.Tx) error {
		q := db.New(tx)

		// Register mentioned entities up front so their names resolve to ids for the claims below.
		entityIDs := make(map[string]pgtype.UUID, len(in.Entities))
		for _, e := range in.Entities {
			id, err := upsertEntity(ctx, q, in.ProjectID, e.Name, e.Type, e.Aliases)
			if err != nil {
				return err
			}
			entityIDs[e.Name] = id
		}

		// Insert memories, indexing the first memory per source event so a same-event claim can link
		// to it (a claim from an event with no memory is stored standalone, memory_id NULL).
		memoryBySeq := make(map[int64]pgtype.UUID, len(in.Memories))
		for _, m := range in.Memories {
			agent := m.CreatedByAgent
			id, err := q.InsertMemory(ctx, db.InsertMemoryParams{
				ProjectID:      in.ProjectID,
				Kind:           m.Kind,
				Content:        m.Content,
				SourceEventID:  m.SourceEventID,
				CreatedByAgent: &agent,
			})
			if err != nil {
				return fmt.Errorf("insert memory: %w", err)
			}
			if _, ok := memoryBySeq[m.SourceSeq]; !ok {
				memoryBySeq[m.SourceSeq] = id
			}
		}

		// Claims in SourceSeq order: resolve the subject, supersede the current active claim for that
		// subject (last-write-wins), then insert the new one. Pre-generating the id lets the supersede
		// point at this replacement before it exists (the self-FK is deferred, validated at commit).
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
			if _, err := q.SupersedeActiveClaimBySubject(ctx, db.SupersedeActiveClaimBySubjectParams{
				SupersededBy: claimID,
				ProjectID:    in.ProjectID,
				EntityID:     entityID,
				Predicate:    c.Predicate,
			}); err != nil {
				return fmt.Errorf("supersede active claim: %w", err)
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
				Value:         c.Value,
				EventTime:     eventTime,
				SourceEventID: c.SourceEventID,
			}); err != nil {
				return fmt.Errorf("insert claim: %w", err)
			}
		}

		if _, err := q.AdvanceCoveredSeq(ctx, db.AdvanceCoveredSeqParams{
			ProjectID:  in.ProjectID,
			RunID:      in.RunID,
			CoveredSeq: in.CoveredSeq,
		}); err != nil {
			return fmt.Errorf("advance checkpoint: %w", err)
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
