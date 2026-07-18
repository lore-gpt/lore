//go:build integration

package pack

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/workmem"
)

func findPackMetric(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) *dto.Metric {
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

// TestPackMetricsFreshnessAndDegrade drives a pack build with a REAL registry and asserts the read-your-writes
// freshness-lag SLO histogram, the build-duration histogram, and the working-source degrade counter [finding
// 6]. The freshness assertion pins the load-bearing ms->seconds unit conversion exactly: the histogram sum
// must equal FreshnessLagMs/1000.
func TestPackMetricsFreshnessAndDegrade(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	run := seedRun(ctx, t, st, proj)

	insertEvent(ctx, t, st, run, "a", `{}`)
	s2 := insertEvent(ctx, t, st, run, "a", `{}`)
	setCovered(ctx, t, st, run, s2)
	// An uncovered event that has waited → a positive freshness lag and a raw tail.
	insertEvent(ctx, t, st, run, "a", `{"late":true}`)
	time.Sleep(25 * time.Millisecond)

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	// A disabled working-memory stripe makes the working source degrade to a non-live source, exercising the
	// degrade counter as well.
	p := New(newTestHybrid(), workmem.NewDisabled(), WithMetrics(m))
	res := runBuild(ctx, t, st, p, proj, run, Request{Query: "x", MinSeq: s2, Limit: 10})
	if res.FreshnessLagMs <= 0 {
		t.Fatalf("precondition: stale freshness = %d, want > 0", res.FreshnessLagMs)
	}

	fresh := findPackMetric(t, reg, "lore_pack_freshness_lag_seconds", nil)
	if fresh == nil || fresh.GetHistogram().GetSampleCount() != 1 {
		t.Fatalf("freshness histogram: want exactly 1 sample, got %v", fresh)
	}
	if got, want := fresh.GetHistogram().GetSampleSum(), float64(res.FreshnessLagMs)/1000; got != want {
		t.Errorf("freshness histogram sum = %v s, want %v s (the ms->seconds conversion)", got, want)
	}

	if bd := findPackMetric(t, reg, "lore_pack_build_duration_seconds", map[string]string{"working_source": res.WorkingSource}); bd == nil || bd.GetHistogram().GetSampleCount() != 1 {
		t.Errorf("build-duration histogram for working_source=%q: want 1 sample, got %v", res.WorkingSource, bd)
	}
	if res.WorkingSource != "live" {
		if dg := findPackMetric(t, reg, "lore_pack_degrade_total", map[string]string{"working_source": res.WorkingSource}); dg == nil || dg.GetCounter().GetValue() < 1 {
			t.Errorf("degrade counter for working_source=%q not recorded", res.WorkingSource)
		}
	}
}
