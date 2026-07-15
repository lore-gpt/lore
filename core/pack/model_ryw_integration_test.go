//go:build integration

package pack

import (
	"context"
	"strings"
	"testing"

	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
)

// TestBuildServesRawTailWithoutActiveModel proves the read-your-writes guarantee holds before any model is
// pinned. A fresh project whose first consolidation has not run has no active embedding model, so the
// distilled read path returns ErrNoActiveModel — but Build treats that as empty distilled retrieval, and the
// raw tail still carries every uncovered event, so a reader sees its own write from the very first event
// rather than a 409. (A genuine model mismatch, by contrast, still propagates.)
func TestBuildServesRawTailWithoutActiveModel(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	// A project with NO active model (fresh, pre-consolidation) and a run with one uncovered event.
	q := db.New(st.Pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "p"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	run := seedRun(ctx, t, st, proj.ID)
	seq := insertEvent(ctx, t, st, run, "planner", `{"note":"predistill_write"}`)

	p := New(newTestHybrid(), workmem.NewDisabled())
	res := runBuild(ctx, t, st, p, proj.ID, run, Request{Query: "anything", MinSeq: seq})

	if len(res.Sources) != 0 {
		t.Errorf("distilled sources = %d, want 0 (no active model → empty distilled retrieval)", len(res.Sources))
	}
	if !strings.Contains(res.Text, "predistill_write") {
		t.Errorf("pack missing the pre-consolidation write in the raw tail:\n%s", res.Text)
	}
	if got := rawTailSeqs(res.Text); len(got) != 1 || got[0] != seq {
		t.Errorf("raw-tail seqs = %v, want [%d] (read-your-writes from the first event)", got, seq)
	}
}
