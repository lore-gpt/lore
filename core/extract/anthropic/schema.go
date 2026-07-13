package anthropic

import (
	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// systemPrompt is the fixed instruction block. It is identical across passes, so
// it forms the cacheable prefix (see the CacheControl breakpoint in Extract): the
// model reuses it and only the trailing events count as fresh input.
const systemPrompt = `You extract durable, reusable memory from the event log of a team of software agents collaborating on a shared task. The events are the raw record of what the agents did and said. Distil only what a teammate joining later would need to know; ignore transient machine chatter such as tool logs, stack traces, and raw command output.

Return your result ONLY by calling the record_extraction tool. Do not write prose.

Produce three kinds of output:

- memories: standalone, self-contained statements of fact worth remembering. Each has a kind — "semantic" (a durable fact about the world or the project), "episodic" (something that happened: an event or a decision), or "procedural" (how to do something) — concise content, and source_seq set to the seq of the event it was distilled from.

- claims: structured assertions of the form (entity, predicate, value). The entity is the subject's name; the predicate is a short snake_case relationship or attribute; the value is JSON (a string, number, boolean, object, or array). Set event_time (RFC 3339) only when the claim is inherently about a point in time; otherwise omit it. Set source_seq as above. Prefer a claim over a memory for a fact that can change over time — status, ownership, version, dependency — because claims are tracked and superseded deterministically.

- entities: the named things the events are about. Each has a name, a type, and optional aliases. Keep the type vocabulary small and consistent: prefer agent, service, repo, task, decision, document, person, or component over inventing new types.

Every memory and claim MUST carry the source_seq of the specific event it was distilled from, copied from that event's "seq" field. If nothing in the window is worth remembering, return empty arrays.`

// extractionTool is the single tool the model is forced to call. Its input_schema
// is the extraction-result shape, so the tool_use input decodes straight into
// wireResult with no free-form parsing. The claim "value" is intentionally an
// unconstrained JSON value, so the schema is not marked strict.
var extractionTool = anthropicsdk.ToolParam{
	Name:        toolName,
	Description: param.NewOpt("Record the memories, claims, and entities distilled from the run's events."),
	InputSchema: anthropicsdk.ToolInputSchemaParam{
		Properties: map[string]any{
			"memories": map[string]any{
				"type":        "array",
				"description": "Standalone facts worth remembering.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind":       map[string]any{"type": "string", "enum": []string{"semantic", "episodic", "procedural"}},
						"content":    map[string]any{"type": "string"},
						"source_seq": map[string]any{"type": "integer", "description": "seq of the event this was distilled from"},
					},
					"required": []string{"kind", "content", "source_seq"},
				},
			},
			"claims": map[string]any{
				"type":        "array",
				"description": "Structured (entity, predicate, value) assertions.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity":     map[string]any{"type": "string"},
						"predicate":  map[string]any{"type": "string", "description": "short snake_case relationship or attribute"},
						"value":      map[string]any{"description": "the claim value as JSON: string, number, boolean, object, or array"},
						"event_time": map[string]any{"type": "string", "description": "RFC 3339 timestamp; only for inherently temporal claims"},
						"source_seq": map[string]any{"type": "integer", "description": "seq of the event this was distilled from"},
					},
					"required": []string{"entity", "predicate", "value", "source_seq"},
				},
			},
			"entities": map[string]any{
				"type":        "array",
				"description": "The named things the events are about.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":    map[string]any{"type": "string"},
						"type":    map[string]any{"type": "string", "description": "small vocabulary: agent, service, repo, task, decision, document, person, component"},
						"aliases": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
					"required": []string{"name", "type"},
				},
			},
		},
		Required: []string{"memories", "claims", "entities"},
	},
}
