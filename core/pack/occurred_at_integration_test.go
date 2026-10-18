//go:build integration

package pack

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestPackDatesMemoriesFromTheStore is the end-to-end proof of the date prefix: a memory whose row carries
// occurred_at reaches the pack text dated, through whichever retrieval path the read takes, and an undated
// memory beside it renders exactly as before.
func TestPackDatesMemoriesFromTheStore(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	run := seedRun(ctx, t, st, proj)

	dated := insertMemKind(ctx, t, st, proj, sectionEpisodic, "the ukulele lessons started", nil)
	insertMemKind(ctx, t, st, proj, sectionEpisodic, "the ukulele was a birthday gift", nil)
	if _, err := st.Pool.Exec(ctx, `UPDATE memories SET occurred_at = $3 WHERE project_id = $1 AND id = $2`,
		proj, dated, time.Date(2023, time.February, 1, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("date memory: %v", err)
	}

	res := runBuild(ctx, t, st, New(newTestHybrid()), proj, run, Request{Query: "ukulele", Limit: 10})

	if !strings.Contains(res.Text, "] (2023-02-01) the ukulele lessons started  [memory ") {
		t.Errorf("dated memory is not prefixed with its date:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "] the ukulele was a birthday gift  [memory ") {
		t.Errorf("undated memory does not render as before:\n%s", res.Text)
	}
}
