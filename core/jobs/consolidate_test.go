package jobs

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/store/db"
)

func TestNormalizeContent(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"lowercases", "Auth Is Done", "auth is done"},
		{"collapses whitespace", "auth   is\tdone\n", "auth is done"},
		{"strips trailing punctuation", "auth is done.", "auth is done"},
		{"strips trailing punct and space", "auth is done . ", "auth is done"},
		{"keeps interior punctuation", "auth: done, then merged", "auth: done, then merged"},
		{"combined", "  The Auth Service Is DONE!!  ", "the auth service is done"},
		{"all punctuation becomes empty", " ... ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeContent(c.in); got != c.want {
				t.Errorf("normalizeContent(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestContentFingerprint(t *testing.T) {
	fp := func(kind string, names []string, content string) string {
		return string(contentFingerprint(kind, names, content))
	}
	base := fp("semantic", []string{"auth"}, "Auth is done.")
	if len(base) != 32 {
		t.Fatalf("fingerprint length = %d, want 32 (sha256)", len(base))
	}

	// Contents that differ only in what normalization folds share a fingerprint (they would merge).
	if got := fp("semantic", []string{"auth"}, "auth   is done"); got != base {
		t.Error("normalization-equivalent contents in the same context should share a fingerprint")
	}
	// Entity-name order does not matter — the fingerprint sorts internally.
	if got := fp("semantic", []string{"auth", "svc"}, "x"); got != fp("semantic", []string{"svc", "auth"}, "x") {
		t.Error("fingerprint should be independent of entity-name order")
	}
	// Genuinely different content does not (no false merge).
	if fp("semantic", []string{"auth"}, "auth is broken") == base {
		t.Error("distinct contents share a fingerprint; dedup would false-merge")
	}
	// Same content, DIFFERENT entity context → different fingerprint (dedup stays inside an entity bucket).
	if fp("semantic", []string{"payment-svc"}, "Auth is done.") == base {
		t.Error("identical text in a different entity context must not share a fingerprint")
	}
	// Same content, DIFFERENT kind → different fingerprint.
	if fp("episodic", []string{"auth"}, "Auth is done.") == base {
		t.Error("identical text of a different kind must not share a fingerprint")
	}
	// An entity-less memory has an empty context; identical entity-less content still shares a fingerprint.
	if fp("semantic", nil, "note") != fp("semantic", []string{}, "note") {
		t.Error("nil and empty entity sets should fingerprint identically (both empty context)")
	}
	// ...and an entity-less fingerprint differs from the same content in an entity context.
	if fp("semantic", nil, "Auth is done.") == base {
		t.Error("entity-less content must not share a fingerprint with entity-bound content")
	}
	// Field-boundary injectivity: the length prefixes must keep a byte moved across a field boundary from
	// aliasing. Naive concatenation would collide each of these pairs into a false merge.
	if fp("semantic", []string{"authb"}, "X") == fp("semantic", []string{"auth"}, "bX") {
		t.Error("name|content boundary aliased (length prefix ineffective) — distinct memories would false-merge")
	}
	if fp("seman", []string{"ticx"}, "y") == fp("semantic", []string{"x"}, "y") {
		t.Error("kind|name boundary aliased (length prefix ineffective) — distinct memories would false-merge")
	}
}

func TestContextFingerprint(t *testing.T) {
	ctxfp := func(kind string, names []string) string { return string(contextFingerprint(kind, names)) }
	base := ctxfp("semantic", []string{"auth"})
	if len(base) != 32 {
		t.Fatalf("context fingerprint length = %d, want 32 (sha256)", len(base))
	}
	// Entity-name order does not matter — the same bucket regardless of input order.
	if ctxfp("semantic", []string{"auth", "svc"}) != ctxfp("semantic", []string{"svc", "auth"}) {
		t.Error("context fingerprint should be independent of entity-name order")
	}
	// A different entity context or kind is a different bucket.
	if ctxfp("semantic", []string{"payment-svc"}) == base {
		t.Error("a different entity context must be a different bucket")
	}
	if ctxfp("episodic", []string{"auth"}) == base {
		t.Error("a different kind must be a different bucket")
	}
	// nil and empty entity sets are the one shared empty-context bucket.
	if ctxfp("semantic", nil) != ctxfp("semantic", []string{}) {
		t.Error("nil and empty entity sets should share the empty-context bucket")
	}
	// The context fingerprint is the content-less bucket key: two DIFFERENT contents in the same context
	// keep DISTINCT content_hashes (so an exact restatement is still recognised) while landing in ONE
	// bucket (compared by similarity, not grouped apart).
	up := contentFingerprint("semantic", []string{"auth"}, "auth is up")
	down := contentFingerprint("semantic", []string{"auth"}, "auth is down")
	if string(up) == string(down) {
		t.Error("distinct content in one bucket must keep distinct content_hashes")
	}
	// The bucket key is never a content hash (content is always appended to the content preimage), so it
	// cannot collide with a content_hash over the same context — even for empty content.
	if base == string(contentFingerprint("semantic", []string{"auth"}, "")) {
		t.Error("context fingerprint must differ from the content fingerprint of empty content")
	}
	// Kind|name boundary injectivity, as for contentFingerprint.
	if ctxfp("seman", []string{"ticx"}) == ctxfp("semantic", []string{"x"}) {
		t.Error("kind|name boundary aliased in the context fingerprint — distinct buckets would collide")
	}
}

func TestEntityNames(t *testing.T) {
	// Mentions and claim subjects are unioned and de-duplicated: "shared" appears in both, "solo-claim"
	// only as a subject, "solo-mention" only as a mention.
	in := PersistInput{
		Entities: []EntityWrite{{Name: "shared"}, {Name: "solo-mention"}},
		Claims: []ClaimWrite{
			{Entity: "shared", Predicate: "p"},
			{Entity: "solo-claim", Predicate: "q"},
			{Entity: "shared", Predicate: "r"}, // a second claim on the same subject must not double it
		},
	}
	got := entityNames(in)
	sort.Strings(got)
	want := []string{"shared", "solo-claim", "solo-mention"}
	if len(got) != len(want) {
		t.Fatalf("entityNames = %v, want the 3 distinct names %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entityNames = %v, want %v", got, want)
			break
		}
	}
}

// conflictSource is a ready, realtime EventSource with one un-gated event: enough for the worker to
// build a window and reach the persist step.
type conflictSource struct{ ev db.Event }

func (c conflictSource) RunExtractionReadiness(context.Context, db.RunExtractionReadinessParams) (db.RunExtractionReadinessRow, error) {
	return db.RunExtractionReadinessRow{EventCount: 1, IdleSeconds: 0}, nil
}
func (c conflictSource) ListRunEvents(context.Context, db.ListRunEventsParams) ([]db.Event, error) {
	return []db.Event{c.ev}, nil
}
func (c conflictSource) GetRunExtractionState(context.Context, db.GetRunExtractionStateParams) (db.GetRunExtractionStateRow, error) {
	return db.GetRunExtractionStateRow{}, nil // realtime, covered_seq 0
}

type oneMemoryExtractor struct{}

func (oneMemoryExtractor) Extract(context.Context, ext.ExtractInput) (ext.ExtractResult, error) {
	return ext.ExtractResult{Memories: []ext.CandidateMemory{{Kind: "semantic", Content: "x", SourceSeq: 1}}}, nil
}

// conflictPersister always reports the checkpoint-conflict signal, as if a concurrent pass advanced the
// run first.
type conflictPersister struct{}

func (conflictPersister) Persist(context.Context, PersistInput) error { return errCheckpointConflict }
func (conflictPersister) SetRunBatch(context.Context, pgtype.UUID, pgtype.UUID, string, int64) error {
	return nil
}

// errPersister returns an arbitrary (non-conflict) error, standing in for a real persist failure.
type errPersister struct{ err error }

func (e errPersister) Persist(context.Context, PersistInput) error { return e.err }
func (errPersister) SetRunBatch(context.Context, pgtype.UUID, pgtype.UUID, string, int64) error {
	return nil
}

// TestWorkReturnsNilOnCheckpointConflict proves the loser of a concurrent double-delivery ends cleanly:
// when the persister reports the checkpoint moved under it, Work returns nil (not an error), so River
// does not churn the job through a retry — the concurrent winner already owns the window.
func TestWorkReturnsNilOnCheckpointConflict(t *testing.T) {
	src := conflictSource{ev: db.Event{Seq: 1, AgentID: "a", Payload: []byte(`{"memory":"x"}`)}}
	w := NewExtractRunWorker(src, oneMemoryExtractor{}, conflictPersister{}, Debounce{IdleWindow: 0, MaxEvents: 1})
	job := &river.Job[ExtractRunArgs]{Args: ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Errorf("Work on a checkpoint conflict = %v, want nil (clean no-op, no retry)", err)
	}
}

// TestWorkPropagatesNonConflictPersistError is the other half of the conflict mapping: a persist error
// that is NOT the checkpoint conflict must still surface from Work (non-nil), so River retries rather
// than the job silently completing and losing the window. Without this, a mutant that swallows every
// persist error to nil would go unnoticed.
func TestWorkPropagatesNonConflictPersistError(t *testing.T) {
	src := conflictSource{ev: db.Event{Seq: 1, AgentID: "a", Payload: []byte(`{"memory":"x"}`)}}
	sentinel := errors.New("database is on fire")
	w := NewExtractRunWorker(src, oneMemoryExtractor{}, errPersister{err: sentinel}, Debounce{IdleWindow: 0, MaxEvents: 1})
	job := &river.Job[ExtractRunArgs]{Args: ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	err := w.Work(context.Background(), job)
	if err == nil {
		t.Fatal("Work should propagate a non-conflict persist error, got nil (River would wrongly complete the job)")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Work error = %v, want it to wrap the underlying persist error", err)
	}
}
