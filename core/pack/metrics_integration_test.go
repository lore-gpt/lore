//go:build integration

package pack

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/lore-gpt/lore/core/metrics"
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
// freshness-lag SLO histogram and the build-duration histogram. The freshness assertion pins the load-bearing
// ms->seconds unit conversion exactly: the histogram sum must equal FreshnessLagMs/1000.
//
// It also pins that the working-section degrade counter does NOT move. That counter measured a fallback
// between two ways of serving the working section; there is only one way now, read from the pack's own
// transaction, so a non-zero value would mean a degrade path had reappeared. Note what this does and does
// not assert: a counter vector with no children exports no series at all, so the family is simply absent
// here. The check below therefore proves nothing increments it — not that it still appears anywhere.
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
	p := New(newTestHybrid(), WithMetrics(m))
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

	if res.WorkingSource != workingLive {
		t.Errorf("WorkingSource = %q, want %q", res.WorkingSource, workingLive)
	}
	if bd := findPackMetric(t, reg, "lore_pack_build_duration_seconds", map[string]string{"working_source": workingLive}); bd == nil || bd.GetHistogram().GetSampleCount() != 1 {
		t.Errorf("build-duration histogram: want 1 sample under %q, got %v", workingLive, bd)
	}
	// No working-section fallback exists any more, so nothing may increment the degrade counter. A value
	// here would mean the section had gone back to being served from an optional dependency.
	for _, label := range []string{workingLive, "durable", "skipped", "unavailable"} {
		if dg := findPackMetric(t, reg, "lore_pack_degrade_total", map[string]string{"working_source": label}); dg != nil && dg.GetCounter().GetValue() > 0 {
			t.Errorf("degrade counter moved for %q (%v); the working section has no fallback to degrade to", label, dg)
		}
	}
}
