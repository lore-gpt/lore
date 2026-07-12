package jobs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/store/db"
)

// Persister commits one extraction pass: it writes the distilled memories and advances the run's
// checkpoint, and it does both in a single transaction. That atomicity is what makes a coalesced
// pass idempotent — the checkpoint only moves past the events a pass consumed if the rows for those
// events committed, so a retried or re-coalesced pass never double-writes and a crashed one is
// reprocessed cleanly. PGPersister is the OSS implementation; tests supply a fake.
type Persister interface {
	Persist(ctx context.Context, in PersistInput) error
}

// PersistInput is one committed unit of extraction for a run: the memories distilled from its window
// and the checkpoint to advance it to. CoveredSeq is the highest seq the pass READ (gated events
// included), so archived machine chatter is marked consumed and never re-read.
type PersistInput struct {
	ProjectID  pgtype.UUID
	RunID      pgtype.UUID
	CoveredSeq int64
	Memories   []MemoryWrite
}

// MemoryWrite is one memory ready to store, with provenance already resolved from the source event:
// SourceEventID is the event it was distilled from and CreatedByAgent is that event's agent.
type MemoryWrite struct {
	Kind           string
	Content        string
	SourceEventID  pgtype.UUID
	CreatedByAgent string
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

// Persist writes the pass's memories and advances the run checkpoint in one transaction. The
// checkpoint advance is guarded to only move forward, so a duplicate pass is a no-op rather than a
// regression.
func (p *PGPersister) Persist(ctx context.Context, in PersistInput) error {
	return p.store.WithProject(ctx, in.ProjectID, func(tx pgx.Tx) error {
		q := db.New(tx)
		for _, m := range in.Memories {
			agent := m.CreatedByAgent
			if _, err := q.InsertMemory(ctx, db.InsertMemoryParams{
				ProjectID:      in.ProjectID,
				Kind:           m.Kind,
				Content:        m.Content,
				SourceEventID:  m.SourceEventID,
				CreatedByAgent: &agent,
			}); err != nil {
				return fmt.Errorf("insert memory: %w", err)
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
