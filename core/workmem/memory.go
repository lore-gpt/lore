package workmem

import (
	"context"
	"sync"
)

// noopStore is the Store used when no cache is configured (LORE_VALKEY_URL unset): Set drops the value,
// Get finds nothing, and Mode is always Disabled — so hot facts fall through to durable extraction.
type noopStore struct{}

func (noopStore) Set(context.Context, Key, Value) error         { return nil }
func (noopStore) Get(context.Context, Key) (Value, bool, error) { return Value{}, false, nil }
func (noopStore) GetAll(context.Context, string, string) ([]Entry, error) {
	return nil, nil
}
func (noopStore) Mode() Mode { return Disabled }
func (noopStore) Close()     {}

// NewDisabled returns the no-op Store (Mode Disabled): Set drops the value and Get finds nothing, so hot
// facts fall through to durable extraction. It is the safe default for a composition that never injects a
// store, so callers never hold a nil Store.
func NewDisabled() Store { return noopStore{} }

// NewMemory returns an in-memory Store (always Healthy). It backs unit tests and any composition that
// wants working memory without an external cache. It does not implement the idle TTL — expiry is a
// property of the Valkey store, not the port.
func NewMemory() Store {
	return &memStore{runs: make(map[string]map[string]Value)}
}

type memStore struct {
	mu   sync.Mutex
	runs map[string]map[string]Value // (project\x00run) -> field -> value
}

func runKey(projectID, runID string) string { return projectID + "\x00" + runID }

func (m *memStore) Set(_ context.Context, k Key, v Value) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rk := runKey(k.ProjectID, k.RunID)
	run, ok := m.runs[rk]
	if !ok {
		run = make(map[string]Value)
		m.runs[rk] = run
	}
	run[field(k.Entity, k.Predicate)] = v
	return nil
}

func (m *memStore) Get(_ context.Context, k Key) (Value, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.runs[runKey(k.ProjectID, k.RunID)][field(k.Entity, k.Predicate)]
	return v, ok, nil
}

func (m *memStore) GetAll(_ context.Context, projectID, runID string) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.runs[runKey(projectID, runID)]
	out := make([]Entry, 0, len(run))
	for f, v := range run {
		entity, predicate, ok := parseField(f)
		if !ok {
			continue
		}
		out = append(out, Entry{Entity: entity, Predicate: predicate, Value: v})
	}
	return out, nil
}

func (*memStore) Mode() Mode { return Healthy }
func (*memStore) Close()     {}
