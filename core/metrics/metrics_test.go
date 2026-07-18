package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// exercise touches every labeled instrument once with bounded, enum-like values so
// Gather emits a sample per metric family (an unobserved vec produces no output).
// It deliberately uses only the kind of values that appear on the real paths.
func exercise(r *Registry) {
	r.HTTPRequests.WithLabelValues("/v1/pack", "GET", "200").Inc()
	r.HTTPDuration.WithLabelValues("/v1/pack", "GET", "200").Observe(0.1)
	r.HTTPInFlight.Inc()
	r.PackBuildDuration.WithLabelValues("live").Observe(0.05)
	r.PackFreshnessLag.Observe(1.2)
	r.PackDegrade.WithLabelValues("durable").Inc()
	r.PackBudgetExceeded.Inc()
	r.PackRawtailTruncate.Inc()
	r.PackModelMismatch.Inc()
	r.RetrievalLegDuration.WithLabelValues("dense", "ok").Observe(0.02)
	r.RetrievalLegCandidates.WithLabelValues("dense", "ok").Observe(10)
	r.RetrievalDensePath.WithLabelValues("hnsw").Inc()
	r.RetrievalQueryCache.WithLabelValues("miss").Inc()
	r.RetrievalLateEmbedDrop.Inc()
	r.RetrievalLateEmbedErr.Inc()
	r.RetrievalModelMismatch.Inc()
	r.ConsolidationMemories.WithLabelValues("inserted").Inc()
	r.ConsolidationSimilarity.WithLabelValues("near_merge").Observe(0.93)
	r.ConsolidationBucketOverflw.Inc()
	r.ConsolidationLockWait.Observe(0.01)
	r.ConsolidationModelMismatch.Inc()
	r.ConsolidationCheckpointCnf.Inc()
	r.ConsolidationPass.WithLabelValues("committed").Inc()
	r.ExtractEventsIngested.Inc()
	r.ExtractEventsGated.Inc()
	r.ExtractEventsExtracted.Inc()
	r.ExtractStateRouted.WithLabelValues("hot").Inc()
	r.QueueJobs.WithLabelValues("extract_run", "completed").Inc()
	r.QueueJobDuration.WithLabelValues("extract_run").Observe(0.3)
	r.QueueJobWait.WithLabelValues("extract_run").Observe(0.5)
	r.QueueDepth.WithLabelValues("extract_run", "available").Set(3)
	r.QueueOldestJobAge.WithLabelValues("extract_run").Set(12)
	r.WorkmemMode.Set(1)
	r.WorkmemWriteFailures.Inc()
	r.BuildInfo.WithLabelValues("v-test", "go1.25").Set(1)
	r.Up.WithLabelValues("server").Set(1)
}

func TestNewRegistersAndGathers(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := New(reg)
	exercise(r)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := map[string]bool{}
	for _, mf := range families {
		names[mf.GetName()] = true
	}
	// A representative spread: the SLO metric, the HTTP surface, and one from each subsystem group.
	for _, want := range []string{
		"lore_http_requests_total",
		"lore_http_request_duration_seconds",
		"lore_pack_freshness_lag_seconds",
		"lore_retrieval_dense_path_total",
		"lore_consolidation_memories_total",
		"lore_queue_oldest_job_age_seconds",
		"lore_build_info",
	} {
		if !names[want] {
			t.Errorf("metric %q not registered", want)
		}
	}
	// Every name carries the lore_ prefix and a Prometheus-conventional suffix.
	for name := range names {
		if !strings.HasPrefix(name, "lore_") {
			t.Errorf("metric %q lacks the lore_ prefix", name)
		}
	}
}

// TestNoUnboundedLabels is the cardinality guardrail: no instrument may carry a
// high-cardinality identifier as a label (it would explode the series count).
// Tenant/entity/agent visibility is a separate concern (metering), never a label.
func TestNoUnboundedLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	exercise(New(reg))
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	forbidden := map[string]bool{
		"project_id": true, "run_id": true, "agent_id": true, "memory_id": true,
		"entity_id": true, "id": true, "key": true, "user_id": true, "seq": true,
	}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if forbidden[lp.GetName()] {
					t.Errorf("metric %q carries a high-cardinality label %q", mf.GetName(), lp.GetName())
				}
			}
		}
	}
}

func TestNoopIsSafeAndUnexported(t *testing.T) {
	// A no-op registry must accept every call unconditionally and never panic, so
	// instrumentation sites never branch on nil.
	r := NewNoop()
	exercise(r)
	// Two independent New calls must not collide (each owns its registry).
	_ = New(prometheus.NewRegistry())
	_ = New(prometheus.NewRegistry())
}
