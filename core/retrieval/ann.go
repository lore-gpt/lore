// Package retrieval reads a project's memories back by embedding similarity. Its filtered
// approximate-nearest-neighbour retriever is the C1 read path: the tenant is a table partition, the
// remaining filters (scope, trust tier) narrow within it, and the retriever counts the filtered set first
// to choose between an exact scan and the HNSW index — never trusting the planner to cost the ANN, and
// never assuming an index exists.
package retrieval

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

// ErrNoActiveModel is returned when a project has no active embedding model chosen. A read queries one
// model's vectors, so with none selected there is nothing to retrieve — a loud, typed error, never a
// silent empty result, so a caller can tell "no model configured" from "no matches".
var ErrNoActiveModel = errors.New("retrieval: project has no active embedding model")

// exactScanCrossover is the filtered-candidate count at or below which the retriever skips the HNSW index
// and scans exactly. For a small set an exact scan is both faster and perfectly recalled and needs no
// index — so it also carries correctness in the window before the index is built. The value sits inside the
// band the de-risk spike validated; the load-bearing choice is that the retriever COUNTS the filtered set
// first (bounded) and picks the path in code, rather than trusting the planner to cost the ANN.
const exactScanCrossover = 8000

// maxVectorDim bounds the dimension interpolated into the index-backed query (pgvector's own ceiling); it
// is checked before dim — the only value the dynamic SQL formats — is ever interpolated.
const maxVectorDim = 16000

// Path names the strategy a retrieval ran, for observability (a later build wires it to a metric).
type Path string

const (
	// PathExact: the filtered set was small (or no valid index exists) — an exact scan.
	PathExact Path = "exact"
	// PathIterative: a large FILTERED set over a valid index — HNSW with hnsw.iterative_scan=strict_order,
	// so the graph keeps scanning until enough filtered rows are found (filtered ANN without recall collapse).
	PathIterative Path = "iterative"
	// PathHNSW: a large UNFILTERED set over a valid index — plain HNSW (the top-k are all valid, so no
	// iterative rescanning is needed).
	PathHNSW Path = "hnsw"
)

// Filters narrow a retrieval. It is the compiled-ACL slot: L1 sources the scope set from the request, a
// later build sources it from a policy, but the SHAPE — an overlap on scope_keys — stays. Empty Scopes
// means project-wide. IncludeQuarantine is an INTERNAL toggle (default false excludes quarantine-tier
// memories); it surfaces to the public API only behind a downstream policy.
type Filters struct {
	Scopes            []string
	IncludeQuarantine bool
}

// Result is one retrieved memory with its cosine distance to the query.
type Result struct {
	ID       pgtype.UUID
	Content  string
	Kind     string
	Distance float64
}

// Retriever runs the filtered approximate-nearest-neighbour read over a project's embeddings. crossover is
// the exact/index switch point; it defaults to exactScanCrossover and is only lowered by tests, which
// exercise the index path without materialising a large dataset.
type Retriever struct {
	crossover int
}

// New returns a Retriever with the default crossover.
func New() *Retriever { return &Retriever{crossover: exactScanCrossover} }

// Retrieve returns the live memories most similar to queryVec within the project, subject to filters,
// ordered by ascending cosine distance, at most limit rows, plus the Path that ran. It resolves the
// project's single embedding-model space (active_model_id — ErrNoActiveModel if none), then chooses a path
// by bounded candidate cardinality: at or below the crossover an exact scan; above it the HNSW index, but
// only when a VALID index exists (otherwise exact, so a missing or invalid index never silently seq-scans).
// It runs on the caller's tenant transaction (RLS-scoped by project); the index path sets
// hnsw.iterative_scan for the transaction. The query vector's length is the model space's dimension; a
// mismatch against stored vectors fails loudly at the database.
func (r *Retriever) Retrieve(ctx context.Context, tx pgx.Tx, projectID pgtype.UUID, queryVec pgvector.Vector, filters Filters, limit int) ([]Result, Path, error) {
	q := db.New(tx)

	modelID, err := q.GetActiveModelID(ctx, projectID)
	if err != nil {
		return nil, "", fmt.Errorf("resolve active model: %w", err)
	}
	if modelID == nil || *modelID == "" {
		return nil, "", ErrNoActiveModel
	}

	dim := len(queryVec.Slice())
	if dim < 1 || dim > maxVectorDim {
		return nil, "", fmt.Errorf("retrieval: query vector dimension %d out of range [1,%d]", dim, maxVectorDim)
	}

	scopes := filters.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	// Filtered means the query narrows beyond the tenant partition: a scope overlap or the default
	// quarantine exclusion. Only a filtered index scan needs iterative_scan; an unfiltered one is plain HNSW.
	filtered := len(scopes) > 0 || !filters.IncludeQuarantine

	// Count the filtered set, bounded to the crossover: at most exactScanCrossover+1 rows are examined.
	count, err := q.CountRetrievalCandidates(ctx, db.CountRetrievalCandidatesParams{
		ProjectID:         projectID,
		ModelID:           *modelID,
		Scopes:            scopes,
		IncludeQuarantine: filters.IncludeQuarantine,
		ScanLimit:         int32(r.crossover) + 1,
	})
	if err != nil {
		return nil, "", fmt.Errorf("count retrieval candidates: %w", err)
	}

	if count <= int64(r.crossover) {
		results, err := r.exact(ctx, q, projectID, *modelID, scopes, filters.IncludeQuarantine, queryVec, limit)
		return results, PathExact, err
	}

	// Large filtered set: use the index only if a valid one exists; otherwise exact carries correctness.
	hasIndex, err := store.HasValidEmbeddingIndex(ctx, tx, projectID)
	if err != nil {
		return nil, "", fmt.Errorf("check vector index: %w", err)
	}
	if !hasIndex {
		results, err := r.exact(ctx, q, projectID, *modelID, scopes, filters.IncludeQuarantine, queryVec, limit)
		return results, PathExact, err
	}

	if filtered {
		if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = strict_order`); err != nil {
			return nil, "", fmt.Errorf("set iterative scan: %w", err)
		}
	}
	results, err := r.indexScan(ctx, tx, projectID, *modelID, scopes, filters.IncludeQuarantine, queryVec, dim, limit)
	if err != nil {
		return nil, "", err
	}
	if filtered {
		return results, PathIterative, nil
	}
	return results, PathHNSW, nil
}

// exact runs the exact-scan path (a sequential scan ordered by cosine distance): the right choice for a
// small filtered set and the correctness fallback when no valid index exists.
func (r *Retriever) exact(ctx context.Context, q *db.Queries, projectID pgtype.UUID, modelID string, scopes []string, includeQuarantine bool, queryVec pgvector.Vector, limit int) ([]Result, error) {
	rows, err := q.RetrieveExact(ctx, db.RetrieveExactParams{
		QueryVec:          queryVec,
		ProjectID:         projectID,
		ModelID:           modelID,
		Scopes:            scopes,
		IncludeQuarantine: includeQuarantine,
		MaxResults:        int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("exact retrieval: %w", err)
	}
	results := make([]Result, len(rows))
	for i, row := range rows {
		results[i] = Result{ID: row.ID, Content: row.Content, Kind: row.Kind, Distance: row.Distance}
	}
	return results, nil
}

// indexScan runs the HNSW-backed path. Its query is built with the vector dimension interpolated because
// the vector(D) typmod cannot be a bind parameter, and the ORDER BY must match the index's built
// expression ((vec::vector(D)) vector_cosine_ops) exactly or the planner silently falls back to a
// sequential scan. ONLY dim is interpolated — a validated int in [1,maxVectorDim] — and every value,
// project_id included, is a bind parameter, so the query carries no injection surface and its project_id
// predicate is explicit (a third belt under RLS). This is the read twin of EnsureIndex's dynamic DDL.
func (r *Retriever) indexScan(ctx context.Context, tx pgx.Tx, projectID pgtype.UUID, modelID string, scopes []string, includeQuarantine bool, queryVec pgvector.Vector, dim, limit int) ([]Result, error) {
	rows, err := tx.Query(ctx, indexQuery(dim), projectID, modelID, scopes, includeQuarantine, int32(limit), queryVec)
	if err != nil {
		return nil, fmt.Errorf("index retrieval: %w", err)
	}
	defer rows.Close()
	var results []Result
	for rows.Next() {
		var res Result
		if err := rows.Scan(&res.ID, &res.Content, &res.Kind, &res.Distance); err != nil {
			return nil, fmt.Errorf("scan retrieval row: %w", err)
		}
		results = append(results, res)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate retrieval rows: %w", err)
	}
	return results, nil
}

// indexQuery builds the HNSW-backed retrieval SQL for a given vector dimension. The dimension is
// interpolated (the vector(D) typmod cannot be a bind parameter) and the ORDER BY reproduces the index's
// built expression ((vec::vector(D)) vector_cosine_ops) so the planner can use the HNSW index — a drift
// between the two silently demotes the query to a sequential scan (locked by an EXPLAIN test). dim is a
// validated int in [1,maxVectorDim]; every other value is a bind parameter ($1 project_id … $6 query_vec).
func indexQuery(dim int) string {
	return fmt.Sprintf(`
SELECT m.id, m.content, m.kind, (e.vec::vector(%d) <=> $6)::float8 AS distance
FROM memories m
JOIN embeddings e ON e.project_id = m.project_id AND e.memory_id = m.id
WHERE m.project_id = $1 AND e.project_id = $1
  AND e.model_id = $2
  AND m.superseded_by IS NULL AND m.valid_to IS NULL
  AND (cardinality($3::text[]) = 0 OR m.scope_keys && $3::text[])
  AND ($4::bool OR m.trust_tier <> 'quarantine')
ORDER BY e.vec::vector(%d) <=> $6
LIMIT $5::int`, dim, dim)
}
