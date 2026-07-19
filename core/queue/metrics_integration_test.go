//go:build integration

package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
)

// findMetric returns the first sample of metric `name` whose labels are a superset of `want`, or nil.
func findMetric(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) *dto.Metric {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			ok := true
			for k, v := range want {
				if got[k] != v {
					ok = false
					break
				}
			}
			if ok {
				return m
			}
		}
	}
	return nil
}

// TestQueueMetricsCompletedOutcomeAndDepth drives one extract_run job to completion with a REAL registry and
// asserts (1) the WorkerMiddleware recorded outcome=completed and a duration sample [finding 5], and (2) the
// periodic SQL collector's depth gauge reflects the completed job [finding 4] — covering the River-internal
// river_job query and the labels, which no other test exercises (all others use a no-op registry).
func TestQueueMetricsCompletedOutcomeAndDepth(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	w, err := queue.NewWorker(st, ext.FixtureExtractor{}, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), m, tracenoop.NewTracerProvider())
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = w.Stop(stopCtx)
	})

	proj, run := seedProjectRun(ctx, t, st)
	if _, err := db.New(st.Pool).InsertEvent(ctx, db.InsertEventParams{
		RunID: run.ID, AgentID: "a", Payload: []byte(`{"memory":"one"}`),
	}); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	projectID := uuid.UUID(proj.ID.Bytes).String()
	enqueueExtract(ctx, t, w, st, projectID, uuid.UUID(run.ID.Bytes).String())
	waitForCompletedExtractJobs(ctx, t, st, 1, 20*time.Second)

	// Finding 5: the middleware labelled the worked job completed and observed its duration. River writes the
	// completed state only after the middleware's Work returns, so once the job is completed the counter is set.
	if got := findMetric(t, reg, "lore_queue_jobs_total", map[string]string{"kind": "extract_run", "outcome": "completed"}); got == nil {
		t.Fatal("lore_queue_jobs_total{kind=extract_run,outcome=completed} not recorded")
	} else if got.GetCounter().GetValue() < 1 {
		t.Errorf("completed count = %v, want >= 1", got.GetCounter().GetValue())
	}
	if got := findMetric(t, reg, "lore_queue_job_duration_seconds", map[string]string{"kind": "extract_run"}); got == nil || got.GetHistogram().GetSampleCount() < 1 {
		t.Error("lore_queue_job_duration_seconds{kind=extract_run} has no observed sample")
	}

	// Finding 4: the SQL collector's depth gauge reflects the completed job. CollectStats scrapes immediately,
	// so run it and poll until the gauge appears, then stop it.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.CollectStats(cctx, m, 50*time.Millisecond)
	deadline := time.Now().Add(10 * time.Second)
	for findMetric(t, reg, "lore_queue_depth_jobs", map[string]string{"kind": "extract_run", "state": "completed"}) == nil {
		if time.Now().After(deadline) {
			t.Fatal("lore_queue_depth_jobs{kind=extract_run,state=completed} did not appear from the SQL collector")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestQueueMetricsErrorOutcome drives a failing extract_run (a fixture_error payload the extractor rejects)
// and asserts the WorkerMiddleware labelled the attempt outcome=error [finding 5, the error branch].
func TestQueueMetricsErrorOutcome(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	w, err := queue.NewWorker(st, ext.FixtureExtractor{}, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), m, tracenoop.NewTracerProvider())
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = w.Stop(stopCtx)
	})

	proj, run := seedProjectRun(ctx, t, st)
	if _, err := db.New(st.Pool).InsertEvent(ctx, db.InsertEventParams{
		RunID: run.ID, AgentID: "a", Payload: []byte(`{"fixture_error":"unavailable"}`),
	}); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	enqueueExtract(ctx, t, w, st, uuid.UUID(proj.ID.Bytes).String(), uuid.UUID(run.ID.Bytes).String())

	// The first attempt fails, so the middleware records outcome=error before River reschedules the retry.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if got := findMetric(t, reg, "lore_queue_jobs_total", map[string]string{"kind": "extract_run", "outcome": "error"}); got != nil && got.GetCounter().GetValue() >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("lore_queue_jobs_total{kind=extract_run,outcome=error} was not recorded within 20s")
		}
		time.Sleep(150 * time.Millisecond)
	}
}
