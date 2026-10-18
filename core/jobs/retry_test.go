package jobs

import (
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func noJitter() float64 { return 0 }

// TestExtractRetryDelayDoublesFromTenSeconds pins the schedule a failed pass retries on.
func TestExtractRetryDelayDoublesFromTenSeconds(t *testing.T) {
	want := []time.Duration{
		10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 320 * time.Second,
		8 * time.Minute, 8 * time.Minute, // capped, should a larger attempt budget ever reach it
	}
	for i, w := range want {
		if got := extractRetryDelay(i+1, noJitter); got != w {
			t.Errorf("wait after failure %d = %v, want %v", i+1, got, w)
		}
	}
	// A count below one (no recorded error yet) is treated as the first failure, never as no wait at all.
	if got := extractRetryDelay(0, noJitter); got != extractRetryBase {
		t.Errorf("wait after failure 0 = %v, want %v", got, extractRetryBase)
	}
}

// TestExtractRetriesOutlastAShortSlowdown is the claim the schedule exists for: between the first attempt and
// the job being discarded, a pass keeps trying for more than ten minutes. Under River's default the three
// attempts were spent inside two, and a provider slow for three stopped the run's distillation for good.
func TestExtractRetriesOutlastAShortSlowdown(t *testing.T) {
	var waited time.Duration
	for n := 1; n < extractMaxAttempts; n++ {
		waited += extractRetryDelay(n, noJitter)
	}
	if waited < 10*time.Minute {
		t.Errorf("the retries wait %v in all before the job is discarded, want more than ten minutes", waited)
	}
	if got := (ExtractRunArgs{}).InsertOpts().MaxAttempts; got != extractMaxAttempts {
		t.Errorf("InsertOpts().MaxAttempts = %d, want the %d the schedule was sized for", got, extractMaxAttempts)
	}
}

// TestExtractRetryJitterStaysWithinATenth keeps the jitter from turning into a different schedule: it spreads
// the runs one outage failed together, by at most a tenth of the wait.
func TestExtractRetryJitterStaysWithinATenth(t *testing.T) {
	for n := 1; n < extractMaxAttempts; n++ {
		base := extractRetryDelay(n, noJitter)
		if got := extractRetryDelay(n, func() float64 { return 0.9999 }); got < base || got > base+base/10 {
			t.Errorf("wait after failure %d with full jitter = %v, want within [%v, %v]", n, got, base, base+base/10)
		}
	}
}

// TestNextRetryCountsRecordedFailures checks the worker hook River calls: the wait follows how many errors the
// job has recorded — not its attempt number, which has not always survived a snooze, and this job snoozes on
// every debounce.
func TestNextRetryCountsRecordedFailures(t *testing.T) {
	w := &ExtractRunWorker{}
	job := &river.Job[ExtractRunArgs]{JobRow: &rivertype.JobRow{
		Attempt: 9, // inflated on purpose: the schedule must not read it
		Errors:  []rivertype.AttemptError{{Attempt: 1}, {Attempt: 2}},
	}}
	before := time.Now().UTC()
	got := w.NextRetry(job)
	// Two recorded errors: this is the third failure, which waits 40s plus up to a tenth.
	if lo, hi := before.Add(40*time.Second), time.Now().UTC().Add(44*time.Second); got.Before(lo) || got.After(hi) {
		t.Errorf("NextRetry = %v, want between %v and %v (third failure: 40s + jitter)", got, lo, hi)
	}
}

// The other half of the design — that the failures a retry cannot fix never wait on this schedule — is pinned
// where those failures are produced: the worker tests that drive a window to the halving floor
// (TestExtractRunWorker_TruncationAtTheFloorCancelsInsteadOfRetrying and its refusal twin) require a
// JobCancelError, which River finalises without consulting NextRetry.
