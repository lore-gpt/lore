//go:build integration

package pack

import (
	"context"
	"strings"
	"testing"
)

// TestPackWorkingSourceIsConstantAndHonest pins what working_source means now that the working section has
// one home.
//
// The field used to describe which of several places served the section, and it was capable of lying: a pack
// built without a live stripe reported "durable" while shipping no working section at all, because the
// durable fallback it named had no producer. There is nothing left to choose between — the section is read
// from the run's own working facts on the pack's own transaction — so the field is constant, and the thing
// worth guarding is that it never becomes a claim about content.
//
// Concretely: a run with no working facts must still report "live" and simply render no section. Reporting
// something else for "empty" would reintroduce the same confusion from the other direction, and treating an
// empty section as a degrade would make a perfectly healthy run look broken.
func TestPackWorkingSourceIsConstantAndHonest(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)

	// A distilled memory so neither pack is empty overall — otherwise "no working section" would be
	// indistinguishable from "nothing worked at all".
	insertMemKind(ctx, t, st, proj, "semantic", "the deploy pipeline is blue-green", nil)

	t.Run("a run with no working facts reports live and renders no section", func(t *testing.T) {
		run := seedRun(ctx, t, st, proj)

		res := runBuild(ctx, t, st, New(newTestHybrid()), proj, run, Request{Query: "pipeline", Limit: 10})

		if res.WorkingSource != workingLive {
			t.Errorf("WorkingSource = %q, want %q even with no facts", res.WorkingSource, workingLive)
		}
		if strings.Contains(res.Text, "## Working memory") {
			t.Errorf("a run with no working facts must render no working section:\n%s", res.Text)
		}
		if len(res.Sources) == 0 {
			t.Fatalf("precondition: the pack should still carry the distilled memory")
		}
	})

	t.Run("a run with working facts reports live and renders them", func(t *testing.T) {
		run := seedRun(ctx, t, st, proj)
		insertWorkingFact(ctx, t, st, proj, run, "task", "status", `"in_progress"`, 1)

		res := runBuild(ctx, t, st, New(newTestHybrid()), proj, run, Request{Query: "pipeline", Limit: 10})

		if res.WorkingSource != workingLive {
			t.Errorf("WorkingSource = %q, want %q", res.WorkingSource, workingLive)
		}
		if !strings.Contains(res.Text, `task.status = "in_progress"`) {
			t.Errorf("working fact missing from the render:\n%s", res.Text)
		}
	})
}
