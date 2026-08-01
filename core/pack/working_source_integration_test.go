//go:build integration

package pack

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/workmem"
)

// TestPackWorkingSourceReportsOutcomeNotIntent pins the split between what the caller is told and what an
// operator is told.
//
// working_source used to be decided before the durable bucket was even read, so a stripe that was off or down
// reported "durable" — naming a fallback that had served nothing. Nothing in the codebase writes working-kind
// memories, so that was the ordinary case: every pack built without a live stripe claimed a durable snapshot
// and shipped an empty working section. A caller cannot distinguish "a snapshot served it" from "there was no
// working section at all", which is the same failure as a partial answer that looks complete.
//
// So the field now reports the OUTCOME, settled once the bucket is known, and the two CAUSES of a fallback
// (the stripe was not authoritative vs a healthy stripe whose read failed) stay on the metric where an
// operator needs them. This test drives both ends: the outcome on Result, and the cause on a real registry.
func TestPackWorkingSourceReportsOutcomeNotIntent(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	run := seedRun(ctx, t, st, proj)

	// A distilled memory so the pack is otherwise non-empty: this must not be mistaken for a working section.
	insertMemKind(ctx, t, st, proj, "semantic", "the deploy pipeline is blue-green", nil)

	for _, tc := range []struct {
		name string
		// store is the working store the pack is built over.
		store func() workmem.Store
		// seedDurable inserts a durable working-kind memory before the build.
		seedDurable bool
		wantSource  string
		wantCause   string
		wantSection bool
	}{
		{
			name:        "no stripe and no snapshot is unavailable, not durable",
			store:       func() workmem.Store { return workmem.NewDisabled() },
			wantSource:  workingUnavailable,
			wantCause:   workingDurable,
			wantSection: false,
		},
		{
			name:        "a healthy stripe whose read fails, with no snapshot, is also unavailable",
			store:       func() workmem.Store { return fakeWorkmem{mode: workmem.Healthy, err: errors.New("cache boom")} },
			wantSource:  workingUnavailable,
			wantCause:   workingSkipped,
			wantSection: false,
		},
		{
			name:        "a snapshot that actually serves the section reports durable",
			store:       func() workmem.Store { return workmem.NewDisabled() },
			seedDurable: true,
			wantSource:  workingDurable,
			wantCause:   workingDurable,
			wantSection: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh project per case: the durable snapshot must not leak between sub-tests.
			p := seedProject(ctx, t, st, testModel)
			r := seedRun(ctx, t, st, p)
			insertMemKind(ctx, t, st, p, "semantic", "the deploy pipeline is blue-green", nil)
			if tc.seedDurable {
				insertMemKind(ctx, t, st, p, sectionWorking, "task status is durable_snapshot_value", nil)
			}

			reg := prometheus.NewRegistry()
			res := runBuild(ctx, t, st, New(newTestHybrid(), tc.store(), WithMetrics(metrics.New(reg))),
				p, r, Request{Query: "status", Limit: 10})

			if res.WorkingSource != tc.wantSource {
				t.Errorf("WorkingSource = %q, want %q", res.WorkingSource, tc.wantSource)
			}
			// The rendered text is the ground truth for "was there a working section": a source label that
			// disagrees with the body is exactly the dishonesty this guards.
			gotSection := strings.Contains(res.Text, "## Working memory")
			if gotSection != tc.wantSection {
				t.Errorf("working section present = %v, want %v; text:\n%s", gotSection, tc.wantSection, res.Text)
			}
			// The cause survives on the metric, so an operator can still tell the two fallbacks apart.
			if got := degradeCount(t, reg, tc.wantCause); got != 1 {
				t.Errorf("pack degrade counter for cause %q = %v, want 1", tc.wantCause, got)
			}
			if got := degradeCount(t, reg, workingUnavailable); got != 0 {
				t.Errorf("the outcome label must not appear on the cause metric, got %v for %q", got, workingUnavailable)
			}
		})
	}

	// A live stripe reports live and is never relabelled, even though the durable bucket is empty.
	t.Run("a live stripe is unaffected", func(t *testing.T) {
		wm := workmem.NewMemory()
		if err := wm.Set(ctx, workmem.Key{ProjectID: uuidStr(proj), RunID: uuidStr(run), Entity: "task", Predicate: "status"},
			workmem.Value{Value: []byte(`"live_value"`), Seq: 1, Agent: "a"}); err != nil {
			t.Fatalf("set: %v", err)
		}
		reg := prometheus.NewRegistry()
		res := runBuild(ctx, t, st, New(newTestHybrid(), wm, WithMetrics(metrics.New(reg))),
			proj, run, Request{Query: "status", Limit: 10})

		if res.WorkingSource != workingLive {
			t.Errorf("WorkingSource = %q, want live", res.WorkingSource)
		}
		if !strings.Contains(res.Text, `"live_value"`) {
			t.Errorf("live fact missing:\n%s", res.Text)
		}
		// A live build is not a degrade: the counter must not fire at all.
		for _, cause := range []string{workingLive, workingDurable, workingSkipped} {
			if got := degradeCount(t, reg, cause); got != 0 {
				t.Errorf("degrade counter fired for %q on a live build: %v", cause, got)
			}
		}
	})
}

// degradeCount reads the pack degrade counter for one working-source label, or 0 when the series is absent.
func degradeCount(t *testing.T, reg *prometheus.Registry, label string) float64 {
	t.Helper()
	m := findPackMetric(t, reg, "lore_pack_degrade_total", map[string]string{"working_source": label})
	if m == nil {
		return 0
	}
	return m.GetCounter().GetValue()
}
