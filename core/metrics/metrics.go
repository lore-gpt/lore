// Package metrics declares the process's Prometheus instruments as one typed
// Registry, so instrumentation sites reference a struct field instead of a
// label-string literal. It holds only the instrument library (client_golang); the
// concrete *prometheus.Registry and the /metrics HTTP handler live in the binary
// (server/internal/telemetry), so the registry is process-owned and testable, not
// the global default.
//
// Naming follows Prometheus conventions: a lore_<subsystem>_<thing>_<unit> name,
// the base unit in the name (_seconds, _bytes, _total), and histogram buckets
// aligned to the product SLOs so a target reads in a single query. Every label set
// is bounded and enum-like — never a project/run/agent id or free text, which
// would explode the series count (tenant-level visibility is a separate concern).
//
// A NewNoop registry lets every instrumentation site call .Inc()/.Observe()
// unconditionally: a composition with no telemetry registers against a throwaway
// registry and never branches on nil.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Bucket sets, shared where the SLO shape is the same. All are in the metric's
// base unit (seconds), and each spans its SLO threshold so p95 reads directly.
var (
	// latencySeconds spans the request/leg latency SLOs. It includes 0.15 so the
	// pack p95 < 150 ms target sits on a bucket boundary.
	latencySeconds = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.15, 0.25, 0.5, 1, 2.5, 5}
	// freshnessSeconds spans the read-your-writes freshness SLO (p95 < 5 s) up to a
	// minute, the range a stalled extraction backlog drives the lag into.
	freshnessSeconds = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	// queueWaitSeconds spans event-committed to worker-started — the async-side
	// freshness proxy — into the minutes a backlog reaches.
	queueWaitSeconds = []float64{0.05, 0.1, 0.5, 1, 5, 10, 30, 60, 300}
	// candidateCounts brackets a retrieval leg's returned candidate count.
	candidateCounts = []float64{1, 5, 10, 25, 50, 100, 250, 500}
)

// Registry is the process's full set of typed instruments, registered against one
// prometheus.Registerer.
type Registry struct {
	// HTTP surface (labels: route = chi route pattern, never the raw path).
	HTTPRequests *prometheus.CounterVec   // [route, method, status]
	HTTPDuration *prometheus.HistogramVec // [route, method, status]
	HTTPInFlight prometheus.Gauge

	// Pack read path — owns the freshness-lag SLO.
	PackBuildDuration   *prometheus.HistogramVec // [working_source]
	PackFreshnessLag    prometheus.Histogram
	PackDegrade         *prometheus.CounterVec // [working_source]
	PackBudgetExceeded  prometheus.Counter
	PackRawtailTruncate prometheus.Counter
	PackModelMismatch   prometheus.Counter

	// Retrieval.
	RetrievalLegDuration   *prometheus.HistogramVec // [leg, status]
	RetrievalLegCandidates *prometheus.HistogramVec // [leg, status]
	RetrievalDensePath     *prometheus.CounterVec   // [path]
	RetrievalQueryCache    *prometheus.CounterVec   // [result]
	RetrievalLateEmbedDrop prometheus.Counter
	RetrievalLateEmbedErr  prometheus.Counter
	RetrievalModelMismatch prometheus.Counter

	// Consolidation / persist (write path). The dedup FUNNEL detail (per-decision cosine histogram,
	// bucket-overflow, advisory-lock wait) lives in free functions the persister calls and is a follow-up;
	// the pass-level outcome, per-memory outcome, and mismatch/conflict counters below cover write health.
	ConsolidationMemories      *prometheus.CounterVec // [outcome]
	ConsolidationModelMismatch prometheus.Counter
	ConsolidationCheckpointCnf prometheus.Counter
	ConsolidationPass          *prometheus.CounterVec // [result]

	// Extraction / gating.
	ExtractEventsIngested  prometheus.Counter
	ExtractEventsGated     prometheus.Counter
	ExtractEventsExtracted prometheus.Counter
	ExtractStateRouted     *prometheus.CounterVec // [lane]

	// Queue lifecycle.
	QueueJobs         *prometheus.CounterVec   // [kind, outcome]
	QueueJobDuration  *prometheus.HistogramVec // [kind]
	QueueJobWait      *prometheus.HistogramVec // [kind]
	QueueDepth        *prometheus.GaugeVec     // [kind, state]
	QueueOldestJobAge *prometheus.GaugeVec     // [kind]

	// Working-memory stripe.
	WorkmemMode          prometheus.Gauge // 0 disabled, 1 healthy, 2 degraded
	WorkmemWriteFailures prometheus.Counter

	// Process identity (constant 1, carrying build/role labels for joins).
	BuildInfo *prometheus.GaugeVec // [version, go_version]
	Up        *prometheus.GaugeVec // [role]
}

// New registers every instrument against reg and returns the typed Registry. It
// panics on a duplicate registration, which can only be a programming error (two
// New calls against one registry), never runtime input.
func New(reg prometheus.Registerer) *Registry {
	f := promauto.With(reg)
	return &Registry{
		HTTPRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "lore_http_requests_total", Help: "HTTP requests by route, method, and status.",
		}, []string{"route", "method", "status"}),
		HTTPDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "lore_http_request_duration_seconds", Help: "HTTP request duration by route, method, and status.",
			Buckets: latencySeconds,
		}, []string{"route", "method", "status"}),
		HTTPInFlight: f.NewGauge(prometheus.GaugeOpts{
			Name: "lore_http_requests_in_flight", Help: "HTTP requests currently being served.",
		}),

		PackBuildDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "lore_pack_build_duration_seconds", Help: "Context-pack build time (library, excludes HTTP framing).",
			Buckets: latencySeconds,
		}, []string{"working_source"}),
		PackFreshnessLag: f.NewHistogram(prometheus.HistogramOpts{
			Name: "lore_pack_freshness_lag_seconds", Help: "Age of the oldest un-distilled event at pack time (the read-your-writes SLO).",
			Buckets: freshnessSeconds,
		}),
		PackDegrade: f.NewCounterVec(prometheus.CounterOpts{
			Name: "lore_pack_degrade_total", Help: "Packs served with a degraded working-memory source.",
		}, []string{"working_source"}),
		PackBudgetExceeded: f.NewCounter(prometheus.CounterOpts{
			Name: "lore_pack_budget_exceeded_total", Help: "Packs where the token budget dropped distilled memories.",
		}),
		PackRawtailTruncate: f.NewCounter(prometheus.CounterOpts{
			Name: "lore_pack_rawtail_truncated_total", Help: "Packs where the raw-tail cap truncated un-distilled events (a stalled-extraction signal).",
		}),
		PackModelMismatch: f.NewCounter(prometheus.CounterOpts{
			Name: "lore_pack_model_mismatch_total", Help: "Pack reads rejected because the embedder model does not match the project's active model.",
		}),

		RetrievalLegDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "lore_retrieval_leg_duration_seconds", Help: "Per-leg retrieval duration.", Buckets: latencySeconds,
		}, []string{"leg", "status"}),
		RetrievalLegCandidates: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "lore_retrieval_leg_candidates", Help: "Per-leg retrieval candidate count.", Buckets: candidateCounts,
		}, []string{"leg", "status"}),
		RetrievalDensePath: f.NewCounterVec(prometheus.CounterOpts{
			Name: "lore_retrieval_dense_path_total", Help: "Dense-leg query path chosen (exact, iterative, or hnsw).",
		}, []string{"path"}),
		RetrievalQueryCache: f.NewCounterVec(prometheus.CounterOpts{
			Name: "lore_retrieval_query_embed_cache_total", Help: "Query-embedding cache result (hit or miss).",
		}, []string{"result"}),
		RetrievalLateEmbedDrop: f.NewCounter(prometheus.CounterOpts{
			Name: "lore_retrieval_late_embed_drop_total", Help: "Dense legs dropped because the query embedding missed the budget.",
		}),
		RetrievalLateEmbedErr: f.NewCounter(prometheus.CounterOpts{
			Name: "lore_retrieval_late_embed_error_total", Help: "Query-embedding failures observed after the budget (fire-and-forget drain).",
		}),
		RetrievalModelMismatch: f.NewCounter(prometheus.CounterOpts{
			Name: "lore_retrieval_model_mismatch_total", Help: "Retrieval reads rejected on an embedder/active-model mismatch.",
		}),

		ConsolidationMemories: f.NewCounterVec(prometheus.CounterOpts{
			Name: "lore_consolidation_memories_total", Help: "Consolidation outcomes per memory (inserted, exact_merged, near_merged, gray_zone).",
		}, []string{"outcome"}),
		ConsolidationModelMismatch: f.NewCounter(prometheus.CounterOpts{
			Name: "lore_consolidation_model_mismatch_total", Help: "Consolidation passes failed because the embedder model does not match the project's active model (write side).",
		}),
		ConsolidationCheckpointCnf: f.NewCounter(prometheus.CounterOpts{
			Name: "lore_consolidation_checkpoint_conflict_total", Help: "Consolidation checkpoint CAS losses (a concurrent pass won).",
		}),
		ConsolidationPass: f.NewCounterVec(prometheus.CounterOpts{
			Name: "lore_consolidation_pass_total", Help: "Extraction-to-consolidation pass outcomes (committed, checkpoint_conflict, error).",
		}, []string{"result"}),

		ExtractEventsIngested: f.NewCounter(prometheus.CounterOpts{
			Name: "lore_extract_events_ingested_total", Help: "Events entering an extraction pass.",
		}),
		ExtractEventsGated: f.NewCounter(prometheus.CounterOpts{
			Name: "lore_extract_events_gated_total", Help: "Events gated out of extraction (filtered before the model); pass-rate = extracted / (extracted + gated).",
		}),
		ExtractEventsExtracted: f.NewCounter(prometheus.CounterOpts{
			Name: "lore_extract_events_extracted_total", Help: "Events that passed the gate into the extraction model.",
		}),
		ExtractStateRouted: f.NewCounterVec(prometheus.CounterOpts{
			Name: "lore_extract_state_routed_total", Help: "kind:state events routed to a lane (hot working-memory or durable).",
		}, []string{"lane"}),

		QueueJobs: f.NewCounterVec(prometheus.CounterOpts{
			Name: "lore_queue_jobs_total", Help: "Worked jobs by kind and outcome.",
		}, []string{"kind", "outcome"}),
		QueueJobDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "lore_queue_job_duration_seconds", Help: "Job execution duration by kind.", Buckets: latencySeconds,
		}, []string{"kind"}),
		QueueJobWait: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "lore_queue_job_wait_seconds", Help: "Time from job availability to work start, by kind (the async freshness proxy).", Buckets: queueWaitSeconds,
		}, []string{"kind"}),
		QueueDepth: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "lore_queue_depth_jobs", Help: "Jobs in the queue by kind and state (periodic scrape).",
		}, []string{"kind", "state"}),
		QueueOldestJobAge: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "lore_queue_oldest_job_age_seconds", Help: "Age of the oldest available job by kind (extraction falling behind ingest).",
		}, []string{"kind"}),

		WorkmemMode: f.NewGauge(prometheus.GaugeOpts{
			Name: "lore_workmem_mode", Help: "Working-memory stripe mode: 0 disabled, 1 healthy, 2 degraded.",
		}),
		WorkmemWriteFailures: f.NewCounter(prometheus.CounterOpts{
			Name: "lore_workmem_write_through_failures_total", Help: "Post-commit working-memory write-through failures (durable store stays authoritative).",
		}),

		BuildInfo: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "lore_build_info", Help: "Build identity; constant 1, labels carry the version.",
		}, []string{"version", "go_version"}),
		Up: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "lore_up", Help: "Process liveness by role; constant 1 while running.",
		}, []string{"role"}),
	}
}

// NewNoop returns a Registry backed by a private throwaway registry, so a
// composition without telemetry can call every instrument unconditionally and
// nothing is exported. It never panics.
func NewNoop() *Registry { return New(prometheus.NewRegistry()) }
