// Package provision bootstraps a tenant: it creates an organization, a project under it, the project's
// memories/embeddings partitions, and one API key, in the order the write path needs. It is the programmatic
// form of the bootstrap the quickstart automates — `lore provision` and the compose one-shot provision
// service both call Provision — so a first project is one command instead of hand-run seed SQL.
package provision

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lore-gpt/lore/core/apikey"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

// Result is the outcome of a successful provision: the created ids and a freshly minted API key. The token is
// shown once and never stored (only its hash is), so a caller that loses it must mint a new key.
type Result struct {
	OrgID     string
	ProjectID string
	Token     string // raw bearer token — shown once, not recoverable
	KeyPrefix string // non-secret leading characters, for later recognition
}

// Provision creates an organization, a project under it, its memories/embeddings partitions, and one API
// key, all in a single transaction so a failure leaves nothing behind — no orphan organization, and no
// project whose partitions or key never landed. It is NOT idempotent: each call mints a fresh organization,
// project, and key. A caller that must run at most once (the compose one-shot service) guards on the
// credentials file it writes, not on this function. The partitions must exist before the first memory write,
// so they are created here rather than lazily; the per-partition vector index is deferred (it needs a chosen
// model and CREATE INDEX CONCURRENTLY, which a background job handles).
func Provision(ctx context.Context, pool *pgxpool.Pool, orgName, projectName string) (Result, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.New(tx)

	org, err := q.InsertOrganization(ctx, orgName)
	if err != nil {
		return Result{}, fmt.Errorf("create organization: %w", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: projectName})
	if err != nil {
		return Result{}, fmt.Errorf("create project: %w", err)
	}
	if err := store.CreateProjectPartitions(ctx, tx, proj.ID); err != nil {
		return Result{}, fmt.Errorf("create partitions: %w", err)
	}

	token, hash, prefix, err := apikey.New()
	if err != nil {
		return Result{}, fmt.Errorf("mint key: %w", err)
	}
	if _, err := q.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		ProjectID: proj.ID,
		KeyPrefix: &prefix,
		KeyHash:   hash,
	}); err != nil {
		return Result{}, fmt.Errorf("store key: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit: %w", err)
	}
	return Result{
		OrgID:     uuid.UUID(org.ID.Bytes).String(),
		ProjectID: uuid.UUID(proj.ID.Bytes).String(),
		Token:     token,
		KeyPrefix: prefix,
	}, nil
}
