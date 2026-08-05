package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lore-gpt/lore/core/ext"
)

// TestSmokeLiveExtraction exercises the real Anthropic Messages API end to end.
// It is skipped unless ANTHROPIC_API_KEY is set, so CI stays offline and
// deterministic; the offline tests in extract_test.go cover the adapter's logic.
// Run it deliberately:
//
//	ANTHROPIC_API_KEY=sk-... go test ./core/extract/anthropic -run TestSmokeLiveExtraction -v
func TestSmokeLiveExtraction(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live extraction smoke")
	}

	e, err := New(Config{APIKey: key})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	res, err := e.Extract(ctx, ext.ExtractInput{
		ProjectID: "smoke",
		RunID:     "smoke",
		Events: []ext.InputEvent{
			{Seq: 1, AgentID: "planner", Payload: json.RawMessage(
				`{"message":"Decided to use PostgreSQL with pgvector as the vector store; the auth service now depends on it."}`)},
			{Seq: 2, AgentID: "coder", Payload: json.RawMessage(
				`{"message":"Merged the pgvector migration; the auth service version is now 2.4.0."}`)},
		},
	})
	if err != nil {
		t.Fatalf("live Extract: %v", err)
	}

	if len(res.Memories) == 0 && len(res.Claims) == 0 {
		t.Fatalf("live extraction distilled nothing: %+v", res)
	}
	t.Logf("live extraction: %d memories, %d claims, %d entities",
		len(res.Memories), len(res.Claims), len(res.Entities))

	// Provenance sanity: every candidate must name an event that was in the window.
	for _, m := range res.Memories {
		if m.SourceSeq != 1 && m.SourceSeq != 2 {
			t.Errorf("memory source_seq %d outside window [1,2]: %q", m.SourceSeq, m.Content)
		}
		if m.Content == "" {
			t.Errorf("memory with empty content survived: %+v", m)
		}
	}
	for _, c := range res.Claims {
		if c.SourceSeq != 1 && c.SourceSeq != 2 {
			t.Errorf("claim source_seq %d outside window [1,2]: %s/%s", c.SourceSeq, c.Entity, c.Predicate)
		}
	}
}

// specificsWindow is the window TestSmokeLiveExtractionRecordsThisRunsFacts distils. Six of its ten events
// are an agent restating well-known background — the kind of thing that would be just as true without this
// run having happened — and four carry a value that belongs to this team. Under real load an extractor is
// choosing what to keep, and that ratio is what makes the choice visible.
var specificsWindow = []ext.InputEvent{
	{Seq: 1, AgentID: "researcher", Payload: json.RawMessage(
		`{"message":"Some background on vector indexes for the group: an HNSW index is a multi-layer proximity graph. Search starts at a sparse top layer and descends, so lookups are logarithmic rather than linear in the number of vectors. Recall is traded against build time and memory through the graph degree and the size of the candidate list kept during a search. Approximate indexes are approximate by construction — they do not promise the exact nearest neighbours, only close ones."}`)},
	{Seq: 2, AgentID: "platform", Payload: json.RawMessage(
		`{"message":"Heads up, the search API is rate limited to 500 requests per minute per key. We hit that ceiling during the load test this morning."}`)},
	{Seq: 3, AgentID: "researcher", Payload: json.RawMessage(
		`{"message":"On connection pooling generally: opening a database connection is expensive because it involves a network round trip and, in the Postgres process model, forking a backend. A pool keeps established connections and hands them out, so the cost is paid once. Pools are usually sized around the database's capacity rather than the application's concurrency, since queueing in the application is cheaper than thrashing the database."}`)},
	{Seq: 4, AgentID: "coder", Payload: json.RawMessage(
		`{"message":"For the record, the auth service is running version 2.3.0 in production right now."}`)},
	{Seq: 5, AgentID: "researcher", Payload: json.RawMessage(
		`{"message":"A note on retries: retrying a failed request without backoff can turn a brief dependency wobble into a sustained overload, because every client retries at once. Exponential backoff with jitter spreads the retries out. Retries are only safe for idempotent operations; for anything else the request needs an idempotency key so a duplicate is recognised and ignored."}`)},
	{Seq: 6, AgentID: "release-manager", Payload: json.RawMessage(
		`{"message":"Code freeze for the 3.2 release starts Friday at 18:00 UTC. Nothing merges to main after that."}`)},
	{Seq: 7, AgentID: "researcher", Payload: json.RawMessage(
		`{"message":"Since it came up: database migrations are usually split into expand and contract phases. The expand phase adds the new shape while the old one still works, the application is moved over, and only then does the contract phase remove the old shape. This keeps every intermediate deploy runnable, which matters because a rollback lands on an intermediate state."}`)},
	{Seq: 8, AgentID: "coder", Payload: json.RawMessage(
		`{"message":"Rolled the auth service forward to 2.4.0. 2.3.0 is no longer deployed anywhere."}`)},
	// The last pair is the hardest shape: a general question wrapped around a specific fact. Everything in
	// the exchange except the row count is textbook material, and an extractor that judges the exchange
	// rather than the statement drops the row count with it.
	{Seq: 9, AgentID: "coder", Payload: json.RawMessage(
		`{"message":"Our users table has 12.4 million rows. What is the safest way to add a NOT NULL column to a table that size?"}`)},
	{Seq: 10, AgentID: "researcher", Payload: json.RawMessage(
		`{"message":"Adding a NOT NULL column with a default used to rewrite the whole table under an ACCESS EXCLUSIVE lock. Modern Postgres stores the default in the catalogue instead, so the add itself is fast. Where a computed value is needed, the usual shape is to add the column nullable, backfill it in batches so no single statement holds a long transaction, and only then set NOT NULL — validated separately so the constraint check does not block writes either."}`)},
}

// fabricatedDate matches an ISO calendar date. specificsWindow contains no date at all — only the weekday
// "Friday" — so any date in the output was worked out by the model rather than read from the events.
var fabricatedDate = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

// TestSmokeLiveExtractionRecordsThisRunsFacts pins the three quality contracts an extraction pass has to
// hold, all against one window and one API call. Each subtest names a failure that was observed live, not
// one that was imagined:
//
//   - keeps the specifics — a memory saying the search API "is rate limited" without the limit, or that the
//     freeze "starts Friday" without the hour, cannot answer the question it was stored for.
//   - leaves out general knowledge — six of this window's ten events are an agent restating textbook
//     material. Recording it produced four background memories out of seven and entities like "HNSW index",
//     which in a budgeted pack displace the run's own facts rather than adding to them. The last pair is the
//     hard case: a general question wrapped around a specific fact, where both contracts apply at once and
//     an extractor that judges the exchange instead of the statement satisfies neither.
//   - invents no calendar date — the events say "Friday" and never name a date, yet a pass turned that into
//     a precise timestamp. A manufactured specific is worse than a vague one: it reads as evidence.
//
// The window's shape carries the test. Four terse, all-signal events prove nothing — there is nothing to
// choose between, so everything survives under any instruction; only a window where most of the content is
// worth dropping can tell a good instruction from a bad one.
//
// Honest about what each subtest is worth, since two prompt revisions have now been measured against it:
//
//   - Against the instructions before any of these rules existed, "leaves out general knowledge" and
//     "invents no calendar date" fail every time while "keeps the specifics" passes. So that first one is a
//     guard against losing ground rather than evidence of a gain — specifics were not what was being dropped
//     at this window size. It stays because it is the contract most likely to be broken by an edit chasing
//     brevity.
//   - Against the revision before the general-knowledge rule was made per-statement, "leaves out general
//     knowledge" fails on the last pair alone: asked a direct question, the extractor recorded the textbook
//     answer as though being asked had made it this run's content.
//
// And one thing this window does NOT catch, said plainly so a later reader does not over-trust it. What
// motivated the per-statement rule was measured on real data: a fact stated in passing while asking for
// advice — "I aimed to raise $200", "compatible with my Sony A7R IV" — disappearing along with the advice.
// At ten events the row count survives without that rule, so this test does not reproduce that failure.
// Only a haystack where most of the content is advice does.
//
// It asserts model behaviour, so no offline fake can stand in: a fixture extractor would only re-assert what
// the fixture was written to return. Skipped unless ANTHROPIC_API_KEY is set, and run deliberately:
//
//	ANTHROPIC_API_KEY=sk-... go test ./core/extract/anthropic -run TestSmokeLiveExtractionRecordsThisRunsFacts -v
func TestSmokeLiveExtractionRecordsThisRunsFacts(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live extraction quality smoke")
	}

	e, err := New(Config{APIKey: key})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := e.Extract(ctx, ext.ExtractInput{
		ProjectID: "smoke",
		RunID:     "smoke",
		Events:    specificsWindow,
	})
	if err != nil {
		t.Fatalf("live Extract: %v", err)
	}

	// A specific may land in a memory's prose or in a claim's value, and background material shows up as an
	// entity as readily as a memory, so every assertion reads the whole pass rather than one slice of it.
	var everything strings.Builder
	for _, m := range res.Memories {
		fmt.Fprintf(&everything, "memory[%s] %s\n", m.Kind, m.Content)
	}
	for _, c := range res.Claims {
		fmt.Fprintf(&everything, "claim %s %s %s\n", c.Entity, c.Predicate, c.Value)
	}
	for _, en := range res.Entities {
		fmt.Fprintf(&everything, "entity[%s] %s\n", en.Type, en.Name)
	}
	produced := everything.String()
	got := strings.ToLower(produced)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("everything the pass produced (%d memories, %d claims, %d entities):\n%s",
				len(res.Memories), len(res.Claims), len(res.Entities), produced)
		}
	})

	t.Run("keeps the specifics", func(t *testing.T) {
		for _, want := range []struct {
			what  string
			forms []string // any one surface form counts
		}{
			{"the rate limit itself", []string{"500"}},
			{"the day the freeze starts", []string{"friday"}},
			// The hour is the one specific a model may legitimately reformat, so every plausible rendering
			// passes. What must not happen is the hour vanishing, leaving "the freeze starts Friday".
			{"the hour the freeze starts", []string{"18:00", "18.00", "1800", "6:00", "6 pm", "6pm", "6 p.m."}},
			// Event 8 supersedes event 4's version. The value in effect must survive; the superseded one is
			// deliberately NOT asserted absent, because narrating the change ("rolled forward from 2.3.0 to
			// 2.4.0") is a correct memory. "2.4" also matches a shortened rendering of 2.4.0.
			{"the version in effect after the roll-forward", []string{"2.4"}},
			// Stated while asking a general question. An extractor that judges the exchange rather than
			// the statement drops this with the answer that follows it — measured happening on real data,
			// where a row count, a price and a piece of hardware all vanished this way.
			{"the row count stated inside a general question", []string{"12.4", "12,400,000", "12400000"}},
		} {
			found := false
			for _, form := range want.forms {
				if strings.Contains(got, form) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s survived nowhere in the pass (looked for any of %q)", want.what, want.forms)
			}
		}
	})

	t.Run("leaves out general knowledge", func(t *testing.T) {
		// One marker per background event, chosen so it cannot occur in a memory about this team's own work:
		// nothing the platform, release-manager or coder agents said touches any of these.
		for _, marker := range []string{"hnsw", "proximity graph", "nearest neighb", "connection pool",
			"forking a backend", "backoff", "idempoten", "expand and contract", "expand-contract",
			// From the answer to the row-count question: the advice must go even though the fact it
			// answers must stay.
			"access exclusive", "backfill", "catalogue"} {
			if strings.Contains(got, marker) {
				t.Errorf("the pass recorded background material (%q) — it would have been just as true "+
					"before this run started, and in a budgeted pack it displaces the run's own facts", marker)
			}
		}
	})

	t.Run("invents no calendar date", func(t *testing.T) {
		if date := fabricatedDate.FindString(produced); date != "" {
			t.Errorf("the pass produced the date %q, which appears nowhere in the events — the window says "+
				"only \"Friday\", so this was manufactured", date)
		}
		for _, c := range res.Claims {
			if c.EventTime != nil {
				t.Errorf("claim %s/%s carries event_time %s; no event in this window states a date, so any "+
					"timestamp here is invented", c.Entity, c.Predicate, c.EventTime.Format(time.RFC3339))
			}
		}
	})
}

// TestSmokeLiveBatchExtraction exercises the real Batch API end to end: submit a window, poll until
// the batch ends, then collect and decode the result. Skipped unless ANTHROPIC_API_KEY is set. A
// batch can take minutes, so run it deliberately with a generous test timeout:
//
//	ANTHROPIC_API_KEY=sk-... go test ./core/extract/anthropic -run TestSmokeLiveBatchExtraction -v -timeout 15m
func TestSmokeLiveBatchExtraction(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live batch extraction smoke")
	}

	e, err := New(Config{APIKey: key})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	window := ext.ExtractInput{
		ProjectID: "smoke",
		RunID:     "smoke",
		Events: []ext.InputEvent{
			{Seq: 1, AgentID: "planner", Payload: json.RawMessage(
				`{"message":"Decided to use PostgreSQL with pgvector as the vector store; the auth service now depends on it."}`)},
			{Seq: 2, AgentID: "coder", Payload: json.RawMessage(
				`{"message":"Merged the pgvector migration; the auth service version is now 2.4.0."}`)},
		},
	}

	handle, err := e.SubmitBatch(ctx, window)
	if err != nil {
		t.Fatalf("live SubmitBatch: %v", err)
	}
	t.Logf("submitted batch %q; polling for completion", handle)

	for {
		res, done, err := e.CollectBatch(ctx, handle)
		if err != nil {
			t.Fatalf("live CollectBatch: %v", err)
		}
		if done {
			if len(res.Memories) == 0 && len(res.Claims) == 0 {
				t.Fatalf("live batch extraction distilled nothing: %+v", res)
			}
			t.Logf("live batch extraction: %d memories, %d claims, %d entities",
				len(res.Memories), len(res.Claims), len(res.Entities))
			for _, m := range res.Memories {
				if m.SourceSeq != 1 && m.SourceSeq != 2 {
					t.Errorf("memory source_seq %d outside window [1,2]: %q", m.SourceSeq, m.Content)
				}
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("batch %q did not complete within the timeout: %v", handle, ctx.Err())
		case <-time.After(10 * time.Second):
		}
	}
}
