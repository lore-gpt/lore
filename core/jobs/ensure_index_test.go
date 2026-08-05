package jobs

import (
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// TestEnsureIndexInsertOpts pins the job's queue and unique-state contract: the build runs on the isolated
// low-priority queue, retries enough to self-heal an interrupted CONCURRENTLY build, is unique per project,
// and — deliberately — does NOT include Completed in the unique states, so a later genuine rebuild (e.g.
// after an index is dropped) can be re-enqueued.
func TestEnsureIndexInsertOpts(t *testing.T) {
	if k := (EnsureIndexArgs{}).Kind(); k != "ensure_index" {
		t.Errorf("kind = %q, want ensure_index", k)
	}
	opts := EnsureIndexArgs{}.InsertOpts()
	if opts.Queue != IndexQueue {
		t.Errorf("queue = %q, want %q (isolated low-priority queue)", opts.Queue, IndexQueue)
	}
	if opts.MaxAttempts != 5 {
		t.Errorf("max attempts = %d, want 5 (retry to self-heal an interrupted build)", opts.MaxAttempts)
	}
	if !opts.UniqueOpts.ByArgs {
		t.Error("UniqueOpts.ByArgs = false, want true (one build per project)")
	}
	for _, s := range opts.UniqueOpts.ByState {
		if s == rivertype.JobStateCompleted {
			t.Error("Completed is in the unique ByState set; it must be excluded so a later genuine rebuild can be re-enqueued")
		}
	}
}

// TestEnsureIndexWorkerTimeoutUnbounded pins that the index build opts out of River's default one-minute job
// timeout: that default would cancel a large CREATE INDEX CONCURRENTLY mid-flight, leave an INVALID index,
// and re-cancel on every retry — so the index would never build for exactly the partitions large enough to
// need it. -1 disables the per-job deadline (the context is still cancelled on worker shutdown).
func TestEnsureIndexWorkerTimeoutUnbounded(t *testing.T) {
	if got := (&EnsureIndexWorker{}).Timeout(nil); got != -1 {
		t.Errorf("ensure_index Timeout = %v, want -1 (no per-job deadline for a long index build)", got)
	}
}

// TestExtractRunWorkerTimeoutOutlivesAPass pins extraction's own deadline. Both halves of it carry weight.
//
// Longer than River's default, because that default is one minute and a real pass does not fit inside it: a
// 200-event window of conversational content was measured overrunning it, and since the window is rebuilt
// identically the retry overran it too. Three cancellations discard the job and the run stops distilling for
// good — the same permanent stall a truncated response used to cause, reached by a different road. A
// regression to the default brings that back, disguised as an intermittent provider problem.
//
// Still bounded, unlike the index build's -1, because extraction shares the default queue's worker slots: a
// job that never returns holds one of them for good, whereas an unbounded index build sits alone on its own
// single-worker queue and starves nothing. The two workers want opposite answers for the same reason —
// what else is competing for the slot.
func TestExtractRunWorkerTimeoutOutlivesAPass(t *testing.T) {
	got := (&ExtractRunWorker{}).Timeout(nil)
	if got <= river.JobTimeoutDefault {
		t.Errorf("extract_run Timeout = %v, want longer than River's default %v — a real pass needs more, and "+
			"a deadline that cancels one cancels every identical retry too", got, river.JobTimeoutDefault)
	}
	if got <= 0 {
		t.Errorf("extract_run Timeout = %v; extraction shares the default queue, so an unbounded job would "+
			"hold one of its worker slots for good", got)
	}
}
