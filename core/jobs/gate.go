package jobs

import (
	"encoding/json"
	"fmt"
)

// maxExtractBytes caps the payload an event may carry and still be worth extracting; larger
// payloads are archived without an LLM pass (they are usually dumps or blobs).
const maxExtractBytes = 8 << 10 // 8 KiB

// skipKinds are payload.kind values that never warrant extraction — tool logs, traces, and other
// machine chatter. Such events are kept (raw) but never sent to the model.
var skipKinds = map[string]struct{}{
	"tool_log":    {},
	"tool_result": {},
	"trace":       {},
	"stack_trace": {},
	"log":         {},
	"debug":       {},
	"heartbeat":   {},
}

// gatedReason is the cheap pre-LLM filter (the extraction gate): it returns a non-empty reason when
// an event should be archived without extraction, or "" when the event should reach the extractor.
// It looks only at payload size and a top-level "kind" tag — a fast heuristic, not a parse of the
// content. A payload that is not a JSON object simply has no "kind" and is not gated on that basis.
func gatedReason(payload []byte) string {
	if len(payload) > maxExtractBytes {
		return fmt.Sprintf("size:%d", len(payload))
	}
	var head struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(payload, &head) == nil && head.Kind != "" {
		if _, skip := skipKinds[head.Kind]; skip {
			return "kind:" + head.Kind
		}
	}
	return ""
}
