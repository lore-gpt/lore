package jobs

import (
	"testing"

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
