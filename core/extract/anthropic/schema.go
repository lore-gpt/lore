package anthropic

import (
	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// systemPrompt is the fixed instruction block. It is identical across passes, so
// it forms the cacheable prefix (see the CacheControl breakpoint in Extract): the
// model reuses it and only the trailing events count as fresh input.
//
// Three of its rules exist because the obvious phrasing of the task produces the wrong output, and each
// was written after seeing that failure in real extractions:
//
//   - "Distil what a teammate would need to know" reads as an instruction to summarise, and summarising is
//     where the exact values die: a window saying the nightly build takes 40 minutes comes back as "the
//     build is slow". The number is the memory — a recalled fact stripped of its quantity, date, version or
//     limit cannot answer the question it was stored for.
//   - A model asked for durable facts will happily write down durable facts it already knew. Those are its
//     prior, not this team's memory, and they crowd out the run's own content in a budgeted pack.
//   - When a value changes mid-window, both versions look like facts. Recording the stale one as if it
//     still held is worse than recording nothing, so the newer value is named as the one in effect and
//     claims are pointed at as the shape that supersedes deterministically.
//
// The predicate-reuse line is not stylistic: predicates are the join key for downstream structure, and a
// model inventing a fresh variant per pass fragments the same attribute across several of them.
const systemPrompt = `You extract durable, reusable memory from the event log of a team of software agents collaborating on a shared task. The events are the raw record of what the agents did and said. Distil only what a teammate joining later would need to know; ignore transient machine chatter such as tool logs, stack traces, and raw command output.

Return your result ONLY by calling the record_extraction tool. Do not write prose.

Record only what is true of this team, this project, this run: what they decided, what they built, what state things are in, what happened. General knowledge is not memory. If a statement would sit unchanged in a textbook, a reference manual, or a piece of general advice — how a technology works, what a practice is for, why an approach is sound — leave it out, however much of the window it fills and however confidently an agent stated it. The test: if it could have been written before this run started, it is not this run's memory.

Keep the specifics. The exact detail is usually the whole value of a memory, so carry it across as stated rather than summarising it away: quantities and their units, dates, times and durations, versions, prices, limits and thresholds, and identifiers such as names, paths, and URLs. "The nightly build takes 40 minutes" is worth recording; "the nightly build is slow" is not. When an event states a specific value, that value belongs in the memory's content or the claim's value, never a general description of it.

Never invent a specific the events do not state. Copy values across as they were stated: if an event says "Friday" without naming a date, the memory says "Friday". A calendar date you worked out yourself is a fabrication, and a confident wrong specific does more damage than a vague true one.

When a later event changes a value an earlier one established, the later value is the one in effect: record it, and do not also record the superseded one as if it still held. If one event puts a service on 2.3 and a later one moves it to 2.4, the version to record is 2.4.

Produce three kinds of output:

- memories: standalone, self-contained statements of fact worth remembering. Each has a kind — "semantic" (a durable fact about the project or the things it touches), "episodic" (something that happened: an event or a decision), or "procedural" (how this team does something) — concise content, and source_seq set to the seq of the event it was distilled from. Concise means no padding; it does not mean dropping the details.

- claims: structured assertions of the form (entity, predicate, value). The entity is the subject's name; the predicate is a short snake_case relationship or attribute; the value is JSON (a string, number, boolean, object, or array) holding the stated value itself — a number as a number, a date as a date — rather than a sentence about it. Set event_time (RFC 3339) only when the claim is inherently about a point in time; otherwise omit it. Set source_seq as above. Prefer a claim over a memory for a fact that can change over time — status, ownership, version, quantity, dependency — because claims are tracked and superseded deterministically. Keep the predicate vocabulary small: reuse a predicate you have already used for the same kind of attribute instead of inventing a variant of it.

- entities: the named things this team works on and with. A concept that merely came up in an explanation is not an entity. Each has a name, a type, and optional aliases. Keep the type vocabulary small and consistent: prefer agent, service, repo, task, decision, document, person, or component over inventing new types.

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
						"content":    map[string]any{"type": "string", "description": "the fact itself, carrying the specific values the event stated"},
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
						"value":      map[string]any{"description": "the stated value itself as JSON — string, number, boolean, object, or array — not a sentence describing it"},
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
