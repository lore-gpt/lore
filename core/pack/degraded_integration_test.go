//go:build integration

package pack

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/retrieval"
)

// slowEmbedder is the fixture embedder with a delay, so a test can push the dense leg past the
// partial-result budget deterministically instead of racing a real provider. It honours context
// cancellation so the dropped leg's goroutine is reclaimed when the build returns without it.
type slowEmbedder struct {
	inner ext.FixtureEmbedder
	delay time.Duration
}

func (s slowEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.inner.Embed(ctx, texts)
}

func (s slowEmbedder) Dim() int        { return s.inner.Dim() }
func (s slowEmbedder) ModelID() string { return s.inner.ModelID() }

// TestPackReportsDroppedDenseLeg covers both directions of the client-visible degrade signal.
//
// A budget shorter than the embedding provider's latency is not hypothetical: the shipped budget
// used to be tuned to the offline fixture, so every request against a hosted embedding endpoint
// dropped the dense leg — the pack answered from the lexical leg alone, returned 200, and said
// nothing about it. The response field is what makes that visible to the caller, so the test pins
// that it appears exactly when a leg is dropped and is absent otherwise.
func TestPackReportsDroppedDenseLeg(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	run := seedRun(ctx, t, st, proj)

	// Two memories whose content carries the query term, so the LEXICAL leg can serve them on its
	// own. Without this the "still returns results" half of the assertion would pass vacuously.
	insertMemKind(ctx, t, st, proj, "semantic", "the payments service moved to grpc", nil)
	insertMemKind(ctx, t, st, proj, "semantic", "grpc rollout finished last sprint", nil)
	s1 := insertEvent(ctx, t, st, run, "a", `{}`)
	setCovered(ctx, t, st, run, s1)

	t.Run("dense dropped past the budget is reported, and the pack still answers", func(t *testing.T) {
		slow := retrieval.NewHybrid(
			retrieval.New(),
			slowEmbedder{delay: 750 * time.Millisecond},
			retrieval.WithPartialTimeout(20*time.Millisecond),
		)
		res := runBuild(ctx, t, st, New(slow), proj, run,
			Request{Query: "grpc", MinSeq: s1, Limit: 10})

		if want := []string{"dense"}; !reflect.DeepEqual(res.Degraded, want) {
			t.Errorf("Degraded = %v, want %v", res.Degraded, want)
		}
		// The whole point of a partial result: the surviving legs still answer. A dropped dense leg
		// that also emptied the pack would be an outage dressed up as a degrade.
		if len(res.Sources) == 0 {
			t.Errorf("sources = 0, want the lexical leg to still serve results")
		}
	})

	t.Run("no leg dropped means no degraded field", func(t *testing.T) {
		res := runBuild(ctx, t, st, New(newTestHybrid()), proj, run,
			Request{Query: "grpc", MinSeq: s1, Limit: 10})

		if res.Degraded != nil {
			t.Errorf("Degraded = %v, want nil on a healthy build", res.Degraded)
		}
		if len(res.Sources) == 0 {
			t.Fatalf("precondition: healthy build returned no sources")
		}
	})

	// A budget generous enough for the slow embedder must NOT report a degrade: this pins that the
	// signal tracks the budget, not merely the presence of a slow provider.
	t.Run("a budget above the provider's latency keeps the leg", func(t *testing.T) {
		patient := retrieval.NewHybrid(
			retrieval.New(),
			slowEmbedder{delay: 50 * time.Millisecond},
			retrieval.WithPartialTimeout(3*time.Second),
		)
		res := runBuild(ctx, t, st, New(patient), proj, run,
			Request{Query: "grpc", MinSeq: s1, Limit: 10})

		if res.Degraded != nil {
			t.Errorf("Degraded = %v, want nil when the leg finished inside the budget", res.Degraded)
		}
	})
}

// TestDefaultPartialTimeoutFitsAHostedProvider pins the shipped default against the latency class it
// exists to accommodate. A hosted embedding endpoint answers in hundreds of milliseconds; a default
// below that drops the dense leg on every request, which is exactly the silent failure the
// configurable budget replaced. This is a unit-level guard on the constant, deliberately loose.
func TestDefaultPartialTimeoutFitsAHostedProvider(t *testing.T) {
	if retrieval.DefaultPartialTimeout < time.Second {
		t.Errorf("DefaultPartialTimeout = %s, want >= 1s (a hosted embedding call routinely takes several hundred ms)",
			retrieval.DefaultPartialTimeout)
	}
}
