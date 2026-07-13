package ext

import (
	"context"
	"encoding/json"
	"fmt"
)

// BasicScopePolicy is the OSS default PolicyEngine: it permits an action when
// the caller holds a scope equal to that action or the wildcard "*".
type BasicScopePolicy struct{}

// Authorize implements PolicyEngine.
func (BasicScopePolicy) Authorize(_ context.Context, scopes []string, action string) error {
	for _, s := range scopes {
		if s == "*" || s == action {
			return nil
		}
	}
	return ErrPermissionDenied
}

// lwwReason and fieldMergeReason identify the policy that produced a resolution; the write path records
// the identifier alongside the change.
const (
	lwwReason        = "last-write-wins"
	fieldMergeReason = "field-merge"
)

// LWW is the OSS default Adjudicator: last write wins. Ordering is arrival order — the incoming write is
// the later one (the caller applies writes in arrival order), so it always survives. The conflict's seqs
// are per-run and not comparable across runs, so LWW deliberately does not consult them.
type LWW struct{}

// Resolve implements Adjudicator.
func (LWW) Resolve(_ context.Context, c Conflict) (Resolution, error) {
	return Resolution{Value: c.Incoming, Reason: lwwReason}, nil
}

// FieldMerge is an OSS Adjudicator that shallow-merges two JSON objects, with
// incoming fields overriding current ones. When either side is not a JSON
// object it falls back to last-write-wins.
type FieldMerge struct{}

// Resolve implements Adjudicator.
func (FieldMerge) Resolve(ctx context.Context, c Conflict) (Resolution, error) {
	cur, curOK := decodeObject(c.Current)
	inc, incOK := decodeObject(c.Incoming)
	if !curOK || !incOK {
		return LWW{}.Resolve(ctx, c)
	}
	for k, v := range inc {
		cur[k] = v
	}
	merged, err := json.Marshal(cur)
	if err != nil {
		return Resolution{}, fmt.Errorf("ext: merge conflict values: %w", err)
	}
	return Resolution{Value: merged, Reason: fieldMergeReason}, nil
}

// decodeObject unmarshals raw as a JSON object. It reports false when raw is not
// a JSON object (including JSON null), so callers can fall back.
func decodeObject(raw []byte) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// NoopMetering is the OSS default MeteringSink: it discards measurements. Usage
// for self-hosted deployments is recoverable from local pack logs.
type NoopMetering struct{}

// Record implements MeteringSink.
func (NoopMetering) Record(context.Context, Measurement) error { return nil }
