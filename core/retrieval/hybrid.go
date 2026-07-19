package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
	"go.opentelemetry.io/otel/attribute"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/obs"
	"github.com/lore-gpt/lore/core/store/db"
)

// Hybrid is the C2 read path: it fuses several retrieval legs — dense vector similarity, lexical full-text,
// and (later) entity-graph — into one ranked result by reciprocal rank fusion, so an exact keyword match a
// nearest-neighbour search misses and a semantic match a keyword search misses both surface. It embeds the
// query, runs the legs, fuses, and applies an optional second-pass reranker, all within the caller's tenant
// transaction.
//
// The legs are NOT run as three concurrent database queries: they share one tenant transaction, and a pgx
// transaction is not safe for concurrent use. The latency worth hiding is the external embedding call
// (tens to hundreds of milliseconds); the database legs are cheap. So the embedding runs on its own
// goroutine while the lexical (and entity) legs run on the transaction, and the dense leg joins once the
// vector is ready — or is dropped if it misses the partial-result budget, the query proceeding on whatever
// legs finished. This keeps the read predictable: a slow embedder degrades recall, never blocks the read.
type Hybrid struct {
	dense    *Retriever
	embedder ext.Embedder
	cache    QueryEmbeddingCache
	rerank   Reranker
	legs     []leg // the transaction-side legs (lexical, entity); the dense leg is orchestrated separately
	k        int
	depth    int
	timeout  time.Duration
	logger   *slog.Logger
	metrics  *metrics.Registry
}

// ErrModelMismatch means the composed embedder's model does not match the project's active embedding model,
// so the query vector would live in a different space than the stored vectors — a silent recall collapse if
// allowed. It is a loud configuration error.
var ErrModelMismatch = errors.New("retrieval: embedder model does not match the project's active model")

// partialTimeout is the budget the dense leg has to produce a query embedding and run before the fusion
// proceeds without it. It bounds tail latency from a slow embedding provider: past the budget the read
// returns on the legs that finished (lexical, entity) rather than waiting. The value tracks the read
// latency target for the whole pack.
const partialTimeout = 80 * time.Millisecond

// queryNormVersion is the version of the query-normalisation scheme baked into a cache key. Bumping it when
// normalisation changes invalidates stale cache entries for free (they key under the old version).
const queryNormVersion = 1

// Status values reported per leg for observability.
const (
	statusOK      = "ok"
	statusStub    = "stub"
	statusTimeout = "timeout"
)

// LegStat is the per-leg outcome of one retrieval, for metrics and logging: how many candidates a leg
// returned, how long it took, and its status (a dense path name like "exact"/"iterative"/"hnsw", "ok" for
// a normal leg, "stub" for a not-yet-implemented leg, or "timeout" when the dense leg missed the budget).
type LegStat struct {
	Name    string
	Status  string
	Count   int
	Latency time.Duration
	Cached  bool // dense only: the query embedding was served from cache
}

// leg is one retrieval source. Every real and stub leg implements the same interface, so fusion treats them
// uniformly and adding a source is registering a provider, not rewiring the orchestration. It returns
// candidates best-first, at most depth, scoped to the tenant transaction and the filters.
type leg interface {
	name() string
	retrieve(ctx context.Context, tx pgx.Tx, projectID pgtype.UUID, q queryInput, filters Filters, depth int) ([]candidate, error)
}

// queryInput carries what the transaction-side legs need — the raw query text. The dense leg is
// orchestrated separately (its vector arrives on its own goroutine under the partial-result budget), so it
// does not read this. It stays a struct so a future leg can carry more without changing the leg interface.
type queryInput struct {
	text string
}

// QueryEmbeddingCache caches a query's embedding vector across calls, so a repeated query skips the
// embedding provider. The OSS default is a no-op — every Get misses — so the read path is already
// cache-aware while a real, shared cache lands with a real embedding provider. The key includes the model
// so a model change can never serve a vector from the wrong space.
type QueryEmbeddingCache interface {
	Get(ctx context.Context, key CacheKey) (pgvector.Vector, bool)
	Put(ctx context.Context, key CacheKey, vec pgvector.Vector)
}

// CacheKey identifies a cached query embedding. model_id is mandatory: without it a model switch would
// serve a stale vector from the previous model's space (a silent recall collapse); with it in the key,
// invalidation on a model change is free.
type CacheKey struct {
	ProjectID   pgtype.UUID
	ModelID     string
	NormVersion int
	QueryHash   string
}

// noopQueryCache is the OSS default: it never caches. It keeps the retrieval path cache-aware with zero
// behaviour, so a downstream build swaps in a real cache without touching callers.
type noopQueryCache struct{}

func (noopQueryCache) Get(context.Context, CacheKey) (pgvector.Vector, bool) {
	return pgvector.Vector{}, false
}
func (noopQueryCache) Put(context.Context, CacheKey, pgvector.Vector) {}

// Reranker is the seam for a second-pass reordering of the fused results (for example a cross-encoder that
// scores each candidate against the query). The OSS default returns them unchanged; a downstream build
// swaps in a real reranker. It runs after fusion, on at most the fused head, before the final truncation to
// the caller's limit. Contract: Rerank REORDERS — it must return exactly the same elements it was given (a
// permutation), never adding or dropping. The caller enforces the length and appends the untouched tail
// after the reranked head, so a length change is rejected as a broken reranker.
type Reranker interface {
	Rerank(ctx context.Context, query string, results []HybridResult) ([]HybridResult, error)
}

// identityReranker is the OSS default Reranker: it returns the fused order unchanged.
type identityReranker struct{}

func (identityReranker) Rerank(_ context.Context, _ string, results []HybridResult) ([]HybridResult, error) {
	return results, nil
}

// HybridOption configures a Hybrid at construction.
type HybridOption func(*Hybrid)

// WithQueryCache injects a query-embedding cache. A nil cache is ignored (the no-op default stays).
func WithQueryCache(c QueryEmbeddingCache) HybridOption {
	return func(h *Hybrid) {
		if c != nil {
			h.cache = c
		}
	}
}

// WithReranker injects a second-pass reranker. A nil reranker is ignored (the identity default stays).
func WithReranker(r Reranker) HybridOption {
	return func(h *Hybrid) {
		if r != nil {
			h.rerank = r
		}
	}
}

// WithPartialTimeout overrides the dense-leg partial-result budget. Mainly for tests; a non-positive value
// is ignored.
func WithPartialTimeout(d time.Duration) HybridOption {
	return func(h *Hybrid) {
		if d > 0 {
			h.timeout = d
		}
	}
}

// WithLogger sets the logger. A nil logger is ignored (the default stays).
func WithLogger(l *slog.Logger) HybridOption {
	return func(h *Hybrid) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithHybridMetrics sets the Prometheus instrument set; a nil registry is ignored
// (the no-op default stays), so instrumentation runs unconditionally.
func WithHybridMetrics(m *metrics.Registry) HybridOption {
	return func(h *Hybrid) {
		if m != nil {
			h.metrics = m
		}
	}
}

// NewHybrid builds a hybrid retriever over the dense retriever and the query embedder. The lexical leg is
// live; the entity leg is a registered stub (it returns nothing and contributes nothing to fusion) until
// the entity-memory substrate lands — the fan-out shape is in place so wiring the real leg is filling in a
// provider, not reshaping the pipeline.
func NewHybrid(dense *Retriever, embedder ext.Embedder, opts ...HybridOption) *Hybrid {
	h := &Hybrid{
		dense:    dense,
		embedder: embedder,
		cache:    noopQueryCache{},
		rerank:   identityReranker{},
		legs:     []leg{lexicalLeg{}, entityStubLeg{}},
		k:        rrfK,
		depth:    legDepth,
		timeout:  partialTimeout,
		logger:   slog.Default(),
		metrics:  metrics.NewNoop(),
	}
	for _, o := range opts {
		o(h)
	}
	h.logger.Info("hybrid retriever configured",
		"rrf_k", h.k, "leg_depth", h.depth, "partial_timeout_ms", h.timeout.Milliseconds(), "entity_leg", statusStub)
	return h
}

// Retrieve returns the memories most relevant to queryText within the project, fusing the dense and lexical
// legs by reciprocal rank fusion, subject to filters, at most limit rows, plus a per-leg stat slice for
// observability. It resolves the project's active embedding model (ErrNoActiveModel if none), rejects a
// mismatch between that model and the embedder (ErrModelMismatch), embeds the query with a budget, runs the
// legs on the caller's tenant transaction, fuses, and applies the reranker. A dense leg that misses the
// budget is dropped and the read proceeds on the remaining legs. The budget bounds only the embedding and
// dense leg; the transaction-side legs (lexical, entity) run to completion — they share the one tenant
// transaction, which cannot be used concurrently, so they are not raced against a timer.
func (h *Hybrid) Retrieve(ctx context.Context, tx pgx.Tx, projectID pgtype.UUID, queryText string, filters Filters, limit int) (results []HybridResult, legStats []LegStat, retErr error) {
	// The business span for one hybrid retrieval, a child of pack.build. It records fusion shape (result and leg
	// counts, dense path) as attributes — never the query text. A no-active-model result ends the span cleanly:
	// an empty distilled retrieval is a normal state (a fresh project), not a failure.
	ctx, span := obs.StartSpan(ctx, "retrieval.hybrid")
	defer func() {
		if retErr != nil && !errors.Is(retErr, ErrNoActiveModel) {
			obs.End(span, retErr)
			return
		}
		obs.End(span, nil)
	}()

	q := db.New(tx)

	modelID, err := q.GetActiveModelID(ctx, projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve active model: %w", err)
	}
	if modelID == nil || *modelID == "" {
		return nil, nil, ErrNoActiveModel
	}
	if *modelID != h.embedder.ModelID() {
		h.metrics.RetrievalModelMismatch.Inc()
		return nil, nil, fmt.Errorf("%w: project uses %q, embedder is %q", ErrModelMismatch, *modelID, h.embedder.ModelID())
	}

	// Normalise the query once and use the normalised form for BOTH the embedding and the lexical leg, so the
	// cache key (which hashes the normalised text) always describes exactly what was embedded — a case or
	// whitespace variant can never serve a cached vector from a different input. The reranker still receives
	// the original text: a second-pass scorer wants the user's true query, not the match-normalised form.
	normalized := normalizeQuery(queryText)

	// Embed the query off the transaction so the lexical (and entity) legs run while the embedding provider
	// works. The buffered channel lets the goroutine finish and exit even if the dense leg is dropped for
	// missing the budget.
	embedCh := make(chan embedOutcome, 1)
	go func() {
		vec, cached, err := h.embedQuery(ctx, projectID, *modelID, normalized)
		embedCh <- embedOutcome{vec: vec, cached: cached, err: err}
	}()

	qi := queryInput{text: normalized}
	perLeg := make(map[string][]candidate, len(h.legs)+1)
	stats := make([]LegStat, 0, len(h.legs)+1)

	// Transaction-side legs, serial on the shared tenant tx (a pgx transaction is not concurrency-safe).
	for _, l := range h.legs {
		start := time.Now()
		cands, err := l.retrieve(ctx, tx, projectID, qi, filters, h.depth)
		if err != nil {
			return nil, nil, fmt.Errorf("%s leg: %w", l.name(), err)
		}
		perLeg[l.name()] = cands
		st := LegStat{Name: l.name(), Count: len(cands), Latency: time.Since(start), Status: statusOK}
		if _, stub := l.(entityStubLeg); stub {
			st.Status = statusStub // registered, deferred: contributes nothing to fusion, consumes no budget
		}
		stats = append(stats, st)
	}

	// Dense leg: join once the embedding is ready, or drop it at the budget. It runs on the transaction
	// AFTER the legs above have returned, so the transaction is never used concurrently.
	denseStart := time.Now()
	select {
	case out := <-embedCh:
		if out.err != nil {
			return nil, nil, fmt.Errorf("embed query: %w", out.err)
		}
		cands, path, err := h.denseLeg(ctx, tx, projectID, out.vec, filters, h.depth)
		if err != nil {
			return nil, nil, fmt.Errorf("dense leg: %w", err)
		}
		perLeg["dense"] = cands
		stats = append(stats, LegStat{Name: "dense", Count: len(cands), Latency: time.Since(denseStart), Status: string(path), Cached: out.cached})
	case <-time.After(h.timeout):
		// Dense missed the budget. Proceed without it; a fire-and-forget drain observes the late embedding so
		// a provider FAILURE (as opposed to mere slowness) is still logged rather than silently swallowed. (A
		// real cache would also store a late success for the next query — that lands with the real cache.)
		go h.drainLateEmbed(embedCh)
		stats = append(stats, LegStat{Name: "dense", Latency: time.Since(denseStart), Status: statusTimeout})
	}

	fused := fuse(perLeg, h.k)

	// Rerank the fused head (identity by default), then restore the untouched tail and truncate to limit. A
	// reranker REORDERS its input, so it must return the same elements: a length change is a broken reranker
	// and fails loud rather than silently corrupting the head/tail boundary.
	head, tail := fused, []HybridResult(nil)
	if len(fused) > h.depth {
		head, tail = fused[:h.depth], fused[h.depth:]
	}
	reranked, err := h.rerank.Rerank(ctx, queryText, head)
	if err != nil {
		return nil, nil, fmt.Errorf("rerank: %w", err)
	}
	if len(reranked) != len(head) {
		return nil, nil, fmt.Errorf("rerank returned %d results for %d inputs: a reranker must reorder, not add or drop", len(reranked), len(head))
	}
	reranked = append(reranked, tail...)
	if limit >= 0 && len(reranked) > limit {
		reranked = reranked[:limit]
	}

	span.SetAttributes(
		attribute.Int("results", len(reranked)),
		attribute.Int("legs", len(stats)),
	)
	for _, s := range stats {
		if s.Name == "dense" {
			// Status carries the dense query path (exact|iterative|hnsw) on success, or "timeout" when it missed
			// the budget — the single most useful retrieval attribute for a trace.
			span.SetAttributes(attribute.String("dense.path", s.Status))
		}
	}

	h.logStats(ctx, projectID, stats)
	h.recordLegMetrics(stats)
	return reranked, stats, nil
}

// recordLegMetrics observes each leg's duration and candidate count into Prometheus. The dense leg's Status
// is overloaded — it carries the query PATH (exact|iterative|hnsw) on success, or "timeout" when it missed
// the budget — so the leg histograms map any path value to "ok" (keeping the leg status enum {ok,stub,
// timeout} bounded) while the path and cache result land on their own dense-specific counters.
func (h *Hybrid) recordLegMetrics(stats []LegStat) {
	for _, s := range stats {
		class := legStatusClass(s.Status)
		h.metrics.RetrievalLegDuration.WithLabelValues(s.Name, class).Observe(s.Latency.Seconds())
		h.metrics.RetrievalLegCandidates.WithLabelValues(s.Name, class).Observe(float64(s.Count))
		if s.Name != "dense" {
			continue
		}
		if class == statusTimeout {
			h.metrics.RetrievalLateEmbedDrop.Inc()
			continue
		}
		h.metrics.RetrievalDensePath.WithLabelValues(s.Status).Inc()
		result := "miss"
		if s.Cached {
			result = "hit"
		}
		h.metrics.RetrievalQueryCache.WithLabelValues(result).Inc()
	}
}

// legStatusClass folds a leg's Status into the bounded {ok, stub, timeout} enum for the leg histograms; a
// dense path value (exact|iterative|hnsw) is a healthy leg and folds to "ok".
func legStatusClass(status string) string {
	switch status {
	case statusStub:
		return statusStub
	case statusTimeout:
		return statusTimeout
	default:
		return statusOK
	}
}

// embedOutcome is the result of the off-transaction query embedding.
type embedOutcome struct {
	vec    pgvector.Vector
	cached bool
	err    error
}

// embedQuery returns the query's embedding, from the cache if present, otherwise from the embedder (and
// then cached). The cache is best-effort: the no-op default always misses, and a real cache's miss or
// failure simply falls through to the embedder — a cache never fails a read.
func (h *Hybrid) embedQuery(ctx context.Context, projectID pgtype.UUID, modelID, text string) (pgvector.Vector, bool, error) {
	key := CacheKey{ProjectID: projectID, ModelID: modelID, NormVersion: queryNormVersion, QueryHash: hashQuery(text)}
	if v, ok := h.cache.Get(ctx, key); ok {
		return v, true, nil
	}
	vecs, err := h.embedder.Embed(ctx, []string{text})
	if err != nil {
		return pgvector.Vector{}, false, err
	}
	if len(vecs) != 1 {
		return pgvector.Vector{}, false, fmt.Errorf("embedder returned %d vectors for one text", len(vecs))
	}
	v := pgvector.NewVector(vecs[0])
	h.cache.Put(ctx, key, v)
	return v, false, nil
}

// drainLateEmbed observes an embedding that arrived after the dense leg's budget expired. The dense result
// is intentionally discarded (the read already proceeded without it), but a late FAILURE is logged so a
// degrading embedding provider stays visible rather than being silently swallowed by the timeout path.
func (h *Hybrid) drainLateEmbed(ch <-chan embedOutcome) {
	if out := <-ch; out.err != nil {
		h.metrics.RetrievalLateEmbedErr.Inc()
		h.logger.Warn("query embedding failed after the partial-result budget; dense leg was dropped", "err", out.err)
	}
}

// denseLeg runs the vector-similarity leg by delegating to the frozen filtered-ANN retriever, mapping its
// results to candidates and surfacing the path it took (for the leg stat).
func (h *Hybrid) denseLeg(ctx context.Context, tx pgx.Tx, projectID pgtype.UUID, vec pgvector.Vector, filters Filters, depth int) ([]candidate, Path, error) {
	results, path, err := h.dense.Retrieve(ctx, tx, projectID, vec, filters, depth)
	if err != nil {
		return nil, "", err
	}
	cands := make([]candidate, len(results))
	for i, r := range results {
		cands[i] = candidate{id: r.ID, content: r.Content, kind: r.Kind}
	}
	return cands, path, nil
}

// logStats emits one structured line with every leg's outcome — the leg-latency and result-count visibility
// for now; a real metrics sink consumes the returned LegStat slice later. It is a single aggregate line, not
// a per-leg log, so the stub entity leg never spams the log.
func (h *Hybrid) logStats(ctx context.Context, projectID pgtype.UUID, stats []LegStat) {
	attrs := make([]any, 0, len(stats)*2+1)
	attrs = append(attrs, "project_id", uuidString(projectID))
	for _, s := range stats {
		attrs = append(attrs, s.Name, fmt.Sprintf("%s/%d/%dms", s.Status, s.Count, s.Latency.Milliseconds()))
	}
	h.logger.LogAttrs(ctx, slog.LevelDebug, "hybrid retrieval legs", slogArgs(attrs)...)
}

// lexicalLeg is the full-text retrieval leg: it matches the query terms against memory content and ranks by
// lexical relevance, complementing the dense leg's semantic match.
type lexicalLeg struct{}

func (lexicalLeg) name() string { return "lexical" }

func (lexicalLeg) retrieve(ctx context.Context, tx pgx.Tx, projectID pgtype.UUID, q queryInput, filters Filters, depth int) ([]candidate, error) {
	scopes := filters.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	rows, err := db.New(tx).RetrieveLexical(ctx, db.RetrieveLexicalParams{
		ProjectID:         projectID,
		QueryText:         q.text,
		Scopes:            scopes,
		IncludeQuarantine: filters.IncludeQuarantine,
		MaxResults:        int32(depth),
	})
	if err != nil {
		return nil, err
	}
	cands := make([]candidate, len(rows))
	for i, r := range rows {
		cands[i] = candidate{id: r.ID, content: r.Content, kind: r.Kind}
	}
	return cands, nil
}

// entityStubLeg is the entity-graph leg, registered but not yet implemented: a memory carries no persisted
// entity linkage today (the denormalised entity set is never populated, and the memory->claim->entity path
// is incomplete while a claim's memory link is optional), so it returns nothing and contributes nothing to
// fusion. It exists so the fan-out is already N-leg and the real leg is a provider swap, not a re-architecture.
type entityStubLeg struct{}

func (entityStubLeg) name() string { return "entity" }

func (entityStubLeg) retrieve(context.Context, pgx.Tx, pgtype.UUID, queryInput, Filters, int) ([]candidate, error) {
	return nil, nil
}

// normalizeQuery folds a query to its cache-key form: lower-cased, with runs of whitespace collapsed and
// the ends trimmed. It is intentionally simple; a change to it is versioned by queryNormVersion.
func normalizeQuery(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// hashQuery is the normalised query's content hash, the query part of a cache key.
func hashQuery(s string) string {
	sum := sha256.Sum256([]byte(normalizeQuery(s)))
	return hex.EncodeToString(sum[:])
}

// uuidString renders a set uuid for logging, or "-" if unset.
func uuidString(id pgtype.UUID) string {
	if !id.Valid {
		return "-"
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", id.Bytes[0:4], id.Bytes[4:6], id.Bytes[6:8], id.Bytes[8:10], id.Bytes[10:16])
}

// slogArgs converts an alternating key/value slice to slog.Attr values.
func slogArgs(kv []any) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		key, _ := kv[i].(string)
		attrs = append(attrs, slog.Any(key, kv[i+1]))
	}
	return attrs
}
