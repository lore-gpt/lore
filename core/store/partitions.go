package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// execer is the slice of pgx used to run partition DDL; both *pgxpool.Pool and
// pgx.Tx satisfy it, so a partition can be created in the same transaction that
// creates the project it belongs to.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// memories and embeddings are LIST-partitioned by project_id (migration 0006). There
// is no DEFAULT partition, so a project's rows can only be written after its partition
// exists: CreateProjectPartitions must run before the first memory/embedding write for
// a project, and DropProjectPartitions is the tenant hard-delete (dropping a partition
// discards its rows — and, once built, its vector index — in one shot).
//
// The per-partition HNSW index is not built here: the embedding vector is dimensionless
// until a model is chosen, and building it needs CREATE INDEX CONCURRENTLY (which cannot
// run inside a transaction). That arrives with the vector-index seam in a following
// increment; this helper only manages partition existence.

// CreateProjectPartitions creates the memories and embeddings partitions for a project.
// It is idempotent (a partition that already exists is left alone).
func CreateProjectPartitions(ctx context.Context, db execer, projectID pgtype.UUID) error {
	pid, err := projectIDText(projectID)
	if err != nil {
		return err
	}
	suffix := strings.ReplaceAll(pid, "-", "")
	for _, parent := range []string{"memories", "embeddings"} {
		// parent + suffix are trusted (fixed strings / a validated uuid), so the
		// dynamic identifier and the FOR VALUES literal cannot be injected.
		stmt := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s_p_%s PARTITION OF %s FOR VALUES IN ('%s')`,
			parent, suffix, parent, pid)
		if _, err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("create %s partition: %w", parent, err)
		}
	}
	return nil
}

// DropProjectPartitions removes a project's partitions — the tenant hard-delete. It is
// idempotent. Embeddings is dropped before memories because it references it.
func DropProjectPartitions(ctx context.Context, db execer, projectID pgtype.UUID) error {
	pid, err := projectIDText(projectID)
	if err != nil {
		return err
	}
	suffix := strings.ReplaceAll(pid, "-", "")
	// Each partition is DETACHed before it is dropped: the inbound foreign keys install
	// per-partition enforcement triggers, so a plain DROP TABLE is refused, and DROP TABLE
	// ... CASCADE would take the whole constraint with it (breaking every other tenant's
	// partition). DETACH removes only this partition's trigger, leaving the constraint
	// intact for the rest. Skipping absent partitions keeps the call idempotent.
	for _, parent := range []string{"embeddings", "memories"} {
		leaf := fmt.Sprintf("%s_p_%s", parent, suffix)
		var present bool
		if err := db.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, leaf).Scan(&present); err != nil {
			return fmt.Errorf("check %s partition: %w", parent, err)
		}
		if !present {
			continue
		}
		if _, err := db.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s`, parent, leaf)); err != nil {
			return fmt.Errorf("detach %s partition: %w", parent, err)
		}
		if _, err := db.Exec(ctx, fmt.Sprintf(`DROP TABLE %s`, leaf)); err != nil {
			return fmt.Errorf("drop %s partition: %w", parent, err)
		}
	}
	return nil
}

// projectIDText renders a set project id as its canonical uuid string. It errors on the
// zero value so a partition is never created for an unset project.
func projectIDText(id pgtype.UUID) (string, error) {
	if !id.Valid {
		return "", fmt.Errorf("project id is not set")
	}
	return uuid.UUID(id.Bytes).String(), nil
}
