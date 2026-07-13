package workmem

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// recordingHandler captures slog records so a test can assert what was logged.
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
func (h *recordingHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.recs...)
}

// TestSetHealthyLogsTransitionsOnce proves the transition logging is once-each and flood-free: no log
// before boot is armed, no log when the state does not change, and exactly one line (at the right level)
// per real transition — so a degraded stripe under steady traffic never spams the log.
func TestSetHealthyLogsTransitionsOnce(t *testing.T) {
	h := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := &valkeyStore{}

	// Before initialized is armed, the boot probe's state change is silent (the caller announces it).
	s.setHealthy(true)
	s.setHealthy(false)
	if n := len(h.snapshot()); n != 0 {
		t.Fatalf("pre-boot transitions logged %d lines, want 0", n)
	}

	s.initialized.Store(true) // arm transition logging; healthy is currently false

	s.setHealthy(false) // no change -> silent
	s.setHealthy(true)  // false -> true: one INFO (reachable)
	s.setHealthy(true)  // no change -> silent
	s.setHealthy(false) // true -> false: one WARN (unreachable)
	s.setHealthy(false) // no change -> silent

	recs := h.snapshot()
	if len(recs) != 2 {
		msgs := make([]string, len(recs))
		for i, r := range recs {
			msgs[i] = r.Message
		}
		t.Fatalf("transitions logged %d lines, want 2: %v", len(recs), msgs)
	}
	if recs[0].Level != slog.LevelInfo {
		t.Errorf("recovery transition level = %v, want INFO", recs[0].Level)
	}
	if recs[1].Level != slog.LevelWarn {
		t.Errorf("degrade transition level = %v, want WARN", recs[1].Level)
	}
}
