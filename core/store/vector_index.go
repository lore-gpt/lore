package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// hnswOpclass is the pgvector operator class the embedding HNSW indexes are built with.
// Cosine is the default for text embeddings — and for L2-normalised vectors it ranks
// identically to inner product — so it stays correct whatever model is chosen later, which
// inner product would not without a normalisation guarantee. It lives here as the single
// source of truth, paired with the read path's matching operator (<=>). Note the metric is
// one line to change in code but not in data: each partition's index has to be rebuilt
// (CREATE INDEX CONCURRENTLY), so a change rides the model-migration path, not a live edit.
const hnswOpclass = "vector_cosine_ops"

// defaultMaintenanceWorkMem is the maintenance_work_mem applied for the duration of a
// build. HNSW graph construction is memory-hungry, so a higher ceiling keeps the build in
// memory; it is a ceiling, not an allocation, so an idle value costs nothing.
const defaultMaintenanceWorkMem = "256MB"

// VectorIndex builds and maintains the approximate-nearest-neighbour index over a
// project's embeddings. It is the narrow seam an alternative vector backend plugs into:
// the default keeps vectors in Postgres (one pgvector HNSW index per project partition),
// but the same interface could route a heavy tenant to an external vector store without
// touching callers — so the escape hatch stays a compiled interface, not a rewrite.
type VectorIndex interface {
	// EnsureIndex makes sure the project's embedding partition carries a vector index
	// built for dim-dimensional vectors. It is idempotent.
	EnsureIndex(ctx context.Context, projectID pgtype.UUID, dim int) error
}

// PgVectorIndex is the default VectorIndex: a per-partition pgvector HNSW index living in
// Postgres next to the data. Dropping the project's partition drops the index with it
// (see DropProjectPartitions), so there is no separate teardown method.
type PgVectorIndex struct {
	pool *pgxpool.Pool
	// MaintenanceWorkMem is the maintenance_work_mem set for the duration of a build.
	// Empty falls back to defaultMaintenanceWorkMem.
	MaintenanceWorkMem string
}

// NewPgVectorIndex returns a PgVectorIndex over pool.
func NewPgVectorIndex(pool *pgxpool.Pool) *PgVectorIndex {
	return &PgVectorIndex{pool: pool, MaintenanceWorkMem: defaultMaintenanceWorkMem}
}

var _ VectorIndex = (*PgVectorIndex)(nil)

// EnsureIndex builds the HNSW index on the project's embeddings partition, if it is not
// already there. The embeddings.vec column is dimensionless (a memory can be embedded by
// several models over its life), so the index is built over an expression cast to a fixed
// dim — which also pins every row in the partition to that one model's dimension, the
// single-model-space invariant the read path relies on. dim comes from the caller (the
// project's active embedding model), not from the column.
//
// Unlike the partition helpers, this cannot share a caller's transaction: CREATE INDEX
// CONCURRENTLY (which builds without locking out writes on a live partition) is not
// allowed inside a transaction block, so EnsureIndex takes a dedicated connection from the
// pool and runs in autocommit. The build is forced serial
// (max_parallel_maintenance_workers = 0) because parallel workers share memory through
// /dev/shm, which is only 64MB in a default container and overflows an HNSW build.
func (v *PgVectorIndex) EnsureIndex(ctx context.Context, projectID pgtype.UUID, dim int) error {
	if dim <= 0 {
		return fmt.Errorf("vector dimension must be positive, got %d", dim)
	}
	_, suffix, err := partitionNames(projectID)
	if err != nil {
		return err
	}
	leaf := fmt.Sprintf("embeddings_p_%s", suffix)
	idx := fmt.Sprintf("%s_vec_hnsw", leaf)

	conn, err := v.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for index build: %w", err)
	}
	defer conn.Release()

	// The build settings must land on the SAME connection that runs the build, which is why
	// this holds a dedicated conn rather than issuing pool.Exec calls that each grab a
	// different one. They are session settings, so reset them before the connection returns
	// to the pool (pgxpool does not reset session state on release).
	workMem := v.MaintenanceWorkMem
	if workMem == "" {
		workMem = defaultMaintenanceWorkMem
	}
	if _, err := conn.Exec(ctx, `SELECT set_config('max_parallel_maintenance_workers', '0', false)`); err != nil {
		return fmt.Errorf("force serial index build: %w", err)
	}
	if _, err := conn.Exec(ctx, `SELECT set_config('maintenance_work_mem', $1, false)`, workMem); err != nil {
		return fmt.Errorf("raise maintenance_work_mem: %w", err)
	}
	defer func() {
		// Best-effort restore on a fresh context so cancellation of ctx does not leave the
		// pooled connection carrying the build settings.
		_, _ = conn.Exec(context.Background(), `RESET maintenance_work_mem`)
		_, _ = conn.Exec(context.Background(), `RESET max_parallel_maintenance_workers`)
	}()

	// A CONCURRENTLY build that failed midway leaves an INVALID index behind. IF NOT EXISTS
	// would then skip past it forever, silently demoting every search on this partition to a
	// sequential scan — the exact recall collapse the per-partition index exists to prevent.
	// Clear such a corpse first so the rebuild actually happens.
	var invalid bool
	switch err := conn.QueryRow(ctx, `
		SELECT NOT i.indisvalid
		FROM pg_class c
		JOIN pg_index i ON i.indexrelid = c.oid
		WHERE c.relname = $1 AND c.relnamespace = 'public'::regnamespace`, idx).Scan(&invalid); {
	case errors.Is(err, pgx.ErrNoRows):
		// No index yet — nothing to clear.
	case err != nil:
		return fmt.Errorf("check existing index %s: %w", idx, err)
	case invalid:
		if _, err := conn.Exec(ctx, fmt.Sprintf(`DROP INDEX CONCURRENTLY IF EXISTS %s`, idx)); err != nil {
			return fmt.Errorf("drop invalid index %s: %w", idx, err)
		}
	}

	// leaf, idx and dim derive from a validated uuid and an int, so the interpolated
	// identifiers and dimension carry no injection surface (same footing as the partition
	// DDL). vec and the opclass are fixed.
	stmt := fmt.Sprintf(
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s USING hnsw ((vec::vector(%d)) %s)`,
		idx, leaf, dim, hnswOpclass)
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("build hnsw index on %s: %w", leaf, err)
	}
	return nil
}
