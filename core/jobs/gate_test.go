package jobs

import (
	"strings"
	"testing"
)

func TestGatedReason(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string // "" = not gated; otherwise a prefix the reason must start with
	}{
		{"plain memory is extracted", `{"memory":"x"}`, ""},
		{"no kind is extracted", `{"foo":"bar"}`, ""},
		{"non-object has no kind", `[1,2,3]`, ""},
		{"unknown kind is extracted", `{"kind":"message"}`, ""},
		{"tool_log is gated", `{"kind":"tool_log"}`, "kind:tool_log"},
		{"stack_trace is gated", `{"kind":"stack_trace","x":1}`, "kind:stack_trace"},
		{"heartbeat is gated", `{"kind":"heartbeat"}`, "kind:heartbeat"},
		{"oversized payload is gated regardless of kind", `{"kind":"message","blob":"` + strings.Repeat("a", 9000) + `"}`, "size:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gatedReason([]byte(tc.payload))
			if tc.want == "" && got != "" {
				t.Errorf("gatedReason = %q, want not gated", got)
			}
			if tc.want != "" && !strings.HasPrefix(got, tc.want) {
				t.Errorf("gatedReason = %q, want prefix %q", got, tc.want)
			}
		})
	}
}
