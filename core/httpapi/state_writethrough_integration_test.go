//go:build integration

package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/httpapi"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/workmem"
)

// errBoom is a stand-in failure for the write-through best-effort path.
var errBoom = errors.New("simulated cache failure")

// captureStore is a workmem.Store with a fixed Mode that records (and can fail) writes, for driving the
// write-through matrix without a real cache. onSet runs before the record is kept, so a test can inspect
// database state at the moment Set is called (locking the commit -> Set order).
type captureStore struct {
	mu    sync.Mutex
	mode  workmem.Mode
	err   error
	onSet func(k workmem.Key, v workmem.Value)
	sets  []workmem.Key
}

func (s *captureStore) Set(_ context.Context, k workmem.Key, v workmem.Value) error {
	if s.onSet != nil {
		s.onSet(k, v)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.sets = append(s.sets, k)
	return nil
}
func (*captureStore) Get(context.Context, workmem.Key) (workmem.Value, bool, error) {
	return workmem.Value{}, false, nil
}
func (*captureStore) GetAll(context.Context, string, string) ([]workmem.Entry, error) {
	return nil, nil
}
func (s *captureStore) Mode() workmem.Mode { return s.mode }
func (*captureStore) Close()               {}
func (s *captureStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sets)
}

// recordingHandler captures slog records so a test can assert what the write-through logged.
type recordingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }
func (h *recordingHandler) writethroughFailures() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.recs {
		if r.Message == "workmem_writethrough_failed" {
			out = append(out, r)
		}
	}
	return out
}

// TestStateEventWriteThrough proves the working-memory write path against a real ParadeDB: a healthy
// stripe makes a state fact immediately readable after the 202 (the read-your-writes acceptance
// criterion); a failing write is best-effort (still 202, exactly one identifier-only WARN, never the
// value); a degraded stripe is skipped entirely (no write, no WARN); and the write happens only after the
// event is durably committed.
func TestStateEventWriteThrough(t *testing.T) {
	ctx := context.Background()
	st, q := newStateTestStore(ctx, t)
	projectID, runID := seedProjectRun(ctx, t, st.Pool)
	apiKey, _ := provisionKey(ctx, t, st.Pool, projectID)

	const stateBody = `{"run_id":"%s","agent_id":"researcher","payload":{"kind":"state","entity":"auth","predicate":"status","value":"up"}}`

	// --- Healthy: the fact is readable straight after the 202 (same-run read-your-writes). ---
	t.Run("healthy write-through is immediately readable", func(t *testing.T) {
		mem := workmem.NewMemory()
		handler := httpapi.New(httpapi.Config{
			Pool: st.Pool, Enqueuer: q, DB: st, Queue: q, Version: "test", Workmem: mem,
		}).Handler()

		seq := postEvent(t, handler, apiKey, fmt.Sprintf(stateBody, runID))

		v, ok, err := mem.Get(ctx, workmem.Key{ProjectID: projectID, RunID: runID, Entity: "auth", Predicate: "status"})
		if err != nil || !ok {
			t.Fatalf("Get after 202: ok=%v err=%v — the fact must be readable immediately", ok, err)
		}
		if string(v.Value) != `"up"` || v.Seq != seq || v.Agent != "researcher" {
			t.Errorf("hot fact = {%s, seq %d, %s}, want {\"up\", %d, researcher}", v.Value, v.Seq, v.Agent, seq)
		}
	})

	// --- Healthy but the write fails: still 202, exactly one WARN, identifiers only (no value). ---
	t.Run("write-through failure is best-effort", func(t *testing.T) {
		rec := &recordingHandler{}
		restore := swapDefaultLogger(rec)
		defer restore()

		cs := &captureStore{mode: workmem.Healthy, err: errBoom}
		handler := httpapi.New(httpapi.Config{
			Pool: st.Pool, Enqueuer: q, DB: st, Queue: q, Version: "test", Workmem: cs,
		}).Handler()

		_ = postEvent(t, handler, apiKey, fmt.Sprintf(stateBody, runID)) // still 202 despite the failed write

		failures := rec.writethroughFailures()
		if len(failures) != 1 {
			t.Fatalf("write-through failures logged = %d, want exactly 1", len(failures))
		}
		if failures[0].Level != slog.LevelWarn {
			t.Errorf("write-through failure logged at %v, want WARN", failures[0].Level)
		}
		attrs := attrsOf(failures[0])
		if _, ok := attrs["run_id"]; !ok {
			t.Error("failure log missing run_id identifier")
		}
		if _, ok := attrs["seq"]; !ok {
			t.Error("failure log missing seq identifier")
		}
		for _, banned := range []string{"value", "payload", "entity", "predicate"} {
			if _, leaked := attrs[banned]; leaked {
				t.Errorf("failure log leaked %q — attrs must be identifiers only", banned)
			}
		}
	})

	// --- Degraded: the write-through is skipped entirely; no Set, no WARN. ---
	t.Run("degraded stripe is skipped silently", func(t *testing.T) {
		rec := &recordingHandler{}
		restore := swapDefaultLogger(rec)
		defer restore()

		cs := &captureStore{mode: workmem.Degraded}
		handler := httpapi.New(httpapi.Config{
			Pool: st.Pool, Enqueuer: q, DB: st, Queue: q, Version: "test", Workmem: cs,
		}).Handler()

		_ = postEvent(t, handler, apiKey, fmt.Sprintf(stateBody, runID))

		if n := cs.count(); n != 0 {
			t.Errorf("degraded stripe received %d writes, want 0 (Mode gate must skip)", n)
		}
		if n := len(rec.writethroughFailures()); n != 0 {
			t.Errorf("degraded skip logged %d WARN lines, want 0 (skips are silent)", n)
		}
	})

	// --- Ordering: the write-through runs only after the event is durably committed. ---
	t.Run("write-through follows commit", func(t *testing.T) {
		committed := false
		cs := &captureStore{mode: workmem.Healthy, onSet: func(k workmem.Key, v workmem.Value) {
			// Scope to THIS event by (run, seq): at Set time it must already be visible in a separate
			// connection's snapshot. Scoping to the run alone would be a false-green, since prior sub-tests
			// committed events for the same run — so a commit/Set reorder would still see rows. Under a
			// reorder this event's row is invisible to st.Pool (uncommitted), so the count is 0.
			var n int
			if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE run_id = $1::uuid AND seq = $2`, k.RunID, v.Seq).Scan(&n); err != nil {
				t.Errorf("query events at Set time: %v", err)
				return
			}
			committed = n == 1
		}}
		handler := httpapi.New(httpapi.Config{
			Pool: st.Pool, Enqueuer: q, DB: st, Queue: q, Version: "test", Workmem: cs,
		}).Handler()

		_ = postEvent(t, handler, apiKey, fmt.Sprintf(stateBody, runID))

		if cs.count() != 1 {
			t.Fatalf("healthy write-through Set count = %d, want 1", cs.count())
		}
		if !committed {
			t.Error("event was not yet committed when the write-through ran — commit must precede Set")
		}
	})
}

// newStateTestStore boots a ParadeDB container, runs the app + queue migrations, and returns an open
// store and queue. It mirrors the setup in the events write-path test but stays independent so neither
// test perturbs the other's data.
func newStateTestStore(ctx context.Context, t *testing.T) (*store.Store, *queue.Queue) {
	t.Helper()
	ctr, err := tcpostgres.Run(ctx, paradeDBImage,
		tcpostgres.WithDatabase("lore"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start paradedb container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("store migrations: %v", err)
	}
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := queue.Migrate(ctx, st.Pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	q, err := queue.New(st.Pool)
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}
	return st, q
}

// swapDefaultLogger installs rec as the default slog logger and returns a restore func.
func swapDefaultLogger(rec slog.Handler) func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	return func() { slog.SetDefault(prev) }
}

func attrsOf(r slog.Record) map[string]slog.Value {
	m := make(map[string]slog.Value)
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value
		return true
	})
	return m
}
