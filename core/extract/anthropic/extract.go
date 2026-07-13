// Package anthropic implements ext.Extractor against the Anthropic Messages API,
// using Claude Haiku by default. It is an OSS, bring-your-own-key (BYOK) provider:
// the operator supplies their own Anthropic API key and the extraction pass runs
// on their account. The offline default stays ext.FixtureExtractor; this adapter
// is composed in only when a key is configured.
//
// Two techniques keep the high-volume extraction pass cheap and reliable:
//
//   - Forced tool-use for structured output. The model must call a single tool
//     whose input_schema is the extraction-result shape, so the response is
//     schema-shaped JSON we decode directly — no free-form parsing and no retries
//     to coax valid JSON out of prose.
//   - Prompt-cache discipline. The fixed instruction + schema prefix carries a
//     cache breakpoint and goes first; the variable events go last. Repeated
//     passes reuse the cached prefix, so only the events count as fresh input.
//
// It also implements ext.BatchExtractor: the same request submitted to the Batch
// API for latency-tolerant "economy" extraction (submit now, collect later),
// which a run opts into via its project's extraction mode.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/lore-gpt/lore/core/ext"
)

// DefaultModel is the extraction model: cheap and fast, which suits the
// high-volume, structured extraction pass.
const DefaultModel = string(anthropicsdk.ModelClaudeHaiku4_5)

// defaultMaxTokens caps the structured output. Extraction returns compact JSON
// over a bounded, gated window (the coalescing debounce keeps a pass small), so
// this ceiling leaves comfortable headroom; cost accrues on tokens actually
// generated, not on the ceiling, so a generous bound is free insurance against a
// truncated pass (see the max_tokens handling in Extract).
const defaultMaxTokens = 4096

// toolName is the single tool the model is forced to call; its input schema is
// the extraction-result shape.
const toolName = "record_extraction"

// batchCustomID identifies the single request in a one-request Message Batch, so a collected result
// can be matched back to it (results may return out of request order).
const batchCustomID = "extraction"

// Config configures the Anthropic-backed Extractor.
type Config struct {
	// APIKey is the operator's Anthropic API key (BYOK). Required.
	APIKey string
	// Model overrides the extraction model. Defaults to DefaultModel.
	Model string
	// BaseURL overrides the API endpoint — for a gateway, a proxy, or a test
	// server. Defaults to the SDK's production endpoint.
	BaseURL string
	// MaxTokens overrides the output ceiling. Defaults to defaultMaxTokens.
	MaxTokens int64
	// HTTPClient overrides the HTTP transport. Optional; injected by tests.
	HTTPClient *http.Client
	// MaxRetries overrides the SDK's transient-error retry count. Nil keeps the
	// SDK default; a test sets 0 to fail fast without backoff.
	MaxRetries *int
}

// Extractor is an ext.Extractor backed by the Anthropic Messages API. It also implements
// ext.BatchExtractor, submitting the same request to the Batch API for latency-tolerant extraction.
type Extractor struct {
	client    anthropicsdk.Client
	model     anthropicsdk.Model
	maxTokens int64
}

var (
	_ ext.Extractor      = (*Extractor)(nil)
	_ ext.BatchExtractor = (*Extractor)(nil)
)

// New builds an Anthropic-backed Extractor from cfg. It returns an error when no
// API key is supplied so a misconfigured worker fails at construction rather than
// at first use; the caller keeps the offline fixture in that case.
func New(cfg Config) (*Extractor, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("extract/anthropic: API key is required")
	}
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	if cfg.MaxRetries != nil {
		opts = append(opts, option.WithMaxRetries(*cfg.MaxRetries))
	}

	model := anthropicsdk.Model(cfg.Model)
	if cfg.Model == "" {
		model = anthropicsdk.Model(DefaultModel)
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	return &Extractor{
		client:    anthropicsdk.NewClient(opts...),
		model:     model,
		maxTokens: maxTokens,
	}, nil
}

// Extract distils one window of a run's events into candidate memories, claims,
// and entities. A transient provider or transport failure returns
// ext.ErrExtractorUnavailable (the coalesced job retries); a request the provider
// rejects outright returns a non-retryable error. An empty window makes no API
// call.
func (e *Extractor) Extract(ctx context.Context, in ext.ExtractInput) (ext.ExtractResult, error) {
	if len(in.Events) == 0 {
		return ext.ExtractResult{}, nil
	}
	parts, err := buildParts(in)
	if err != nil {
		return ext.ExtractResult{}, err
	}

	msg, err := e.client.Messages.New(ctx, anthropicsdk.MessageNewParams{
		Model:       e.model,
		MaxTokens:   e.maxTokens,
		Temperature: param.NewOpt(0.0), // extraction should be as deterministic as the model allows
		System:      parts.system,
		Tools:       parts.tools,
		ToolChoice:  parts.toolChoice,
		Messages:    parts.messages,
	})
	if err != nil {
		return ext.ExtractResult{}, mapError(err)
	}
	return e.decodeResult(msg, in.Events)
}

// requestParts holds the pieces shared by the synchronous and batch requests: the fixed system+schema
// prefix (with a cache breakpoint), the forced extraction tool, and the events as the trailing user
// message. Building them once keeps the two paths identical on the wire.
type requestParts struct {
	system     []anthropicsdk.TextBlockParam
	tools      []anthropicsdk.ToolUnionParam
	toolChoice anthropicsdk.ToolChoiceUnionParam
	messages   []anthropicsdk.MessageParam
}

// buildParts assembles the request pieces from one window: the fixed prefix goes first with a cache
// breakpoint so repeated passes reuse the cached tools+system, and the variable events go last.
func buildParts(in ext.ExtractInput) (requestParts, error) {
	userJSON, err := marshalEvents(in.Events)
	if err != nil {
		return requestParts{}, fmt.Errorf("extract/anthropic: encode events: %w", err)
	}
	return requestParts{
		system: []anthropicsdk.TextBlockParam{{
			Text:         systemPrompt,
			CacheControl: anthropicsdk.NewCacheControlEphemeralParam(),
		}},
		tools:      []anthropicsdk.ToolUnionParam{{OfTool: &extractionTool}},
		toolChoice: anthropicsdk.ToolChoiceParamOfTool(toolName),
		messages:   []anthropicsdk.MessageParam{anthropicsdk.NewUserMessage(anthropicsdk.NewTextBlock(userJSON))},
	}, nil
}

// decodeResult turns a completed message into an ExtractResult. It rejects a max_tokens-truncated
// response as a transient miss — the tool input is valid JSON but only partial, and persisting it as
// complete would advance the checkpoint past silently-dropped candidates — and requires the forced
// tool_use block. events is used only for the single-event provenance fallback; it is nil on the batch
// path (the collected result carries no window), where the write path's out-of-window drop is the net.
func (e *Extractor) decodeResult(msg *anthropicsdk.Message, events []ext.InputEvent) (ext.ExtractResult, error) {
	if msg.StopReason == anthropicsdk.StopReasonMaxTokens {
		return ext.ExtractResult{}, fmt.Errorf("%w: response truncated at max_tokens (%d)", ext.ErrExtractorUnavailable, e.maxTokens)
	}
	raw, ok := toolInput(msg, toolName)
	if !ok {
		// The model was forced to call the tool; a response without the tool_use block is unusable.
		return ext.ExtractResult{}, fmt.Errorf("%w: response carried no %s tool call", ext.ErrExtractorUnavailable, toolName)
	}
	var out wireResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return ext.ExtractResult{}, fmt.Errorf("extract/anthropic: decode tool input: %w", err)
	}
	return out.toResult(events), nil
}

// SubmitBatch implements ext.BatchExtractor: it submits the window as a one-request Message Batch and
// returns the batch id as the opaque handle for CollectBatch. The request is identical to Extract's,
// so the same prompt-cache and structured-output discipline applies. A transient provider or transport
// failure returns ext.ErrExtractorUnavailable.
func (e *Extractor) SubmitBatch(ctx context.Context, in ext.ExtractInput) (string, error) {
	if len(in.Events) == 0 {
		return "", fmt.Errorf("extract/anthropic: submit batch on an empty window")
	}
	parts, err := buildParts(in)
	if err != nil {
		return "", err
	}

	batch, err := e.client.Messages.Batches.New(ctx, anthropicsdk.MessageBatchNewParams{
		Requests: []anthropicsdk.MessageBatchNewParamsRequest{{
			CustomID: batchCustomID,
			Params: anthropicsdk.MessageBatchNewParamsRequestParams{
				Model:       e.model,
				MaxTokens:   e.maxTokens,
				Temperature: param.NewOpt(0.0),
				System:      parts.system,
				Tools:       parts.tools,
				ToolChoice:  parts.toolChoice,
				Messages:    parts.messages,
			},
		}},
	})
	if err != nil {
		return "", mapError(err)
	}
	return batch.ID, nil
}

// CollectBatch implements ext.BatchExtractor: it reports whether the batch named by handle has ended
// and, once it has, streams the results and decodes the one matching our request. A batch still
// processing returns done=false so the caller polls again; a transient provider or transport failure
// returns ext.ErrExtractorUnavailable; a batch whose single request did not succeed (errored, canceled,
// expired) returns an error naming the outcome.
func (e *Extractor) CollectBatch(ctx context.Context, handle string) (ext.ExtractResult, bool, error) {
	batch, err := e.client.Messages.Batches.Get(ctx, handle)
	if err != nil {
		return ext.ExtractResult{}, false, mapError(err)
	}
	if batch.ProcessingStatus != anthropicsdk.MessageBatchProcessingStatusEnded {
		return ext.ExtractResult{}, false, nil // still processing; the caller polls again later.
	}

	stream := e.client.Messages.Batches.ResultsStreaming(ctx, handle)
	defer func() { _ = stream.Close() }()
	for stream.Next() {
		item := stream.Current()
		if item.CustomID != batchCustomID {
			continue
		}
		if item.Result.Type != "succeeded" {
			// errored / canceled / expired: the request will not yield a result. Surface it rather than
			// treat an absent result as empty.
			return ext.ExtractResult{}, false, fmt.Errorf("extract/anthropic: batch request did not succeed: %s", item.Result.Type)
		}
		msg := item.Result.Message
		res, err := e.decodeResult(&msg, nil)
		if err != nil {
			return ext.ExtractResult{}, false, err
		}
		return res, true, nil
	}
	if err := stream.Err(); err != nil {
		return ext.ExtractResult{}, false, mapError(err)
	}
	// The batch ended but streamed no result for our request: a provider inconsistency, so retry.
	return ext.ExtractResult{}, false, fmt.Errorf("%w: batch %q returned no result for %q", ext.ErrExtractorUnavailable, handle, batchCustomID)
}

// wireEventInput is one event as presented to the model: its per-run seq (which
// the model echoes back as each candidate's source_seq for provenance), the
// writing agent, and the opaque payload.
type wireEventInput struct {
	Seq     int64           `json:"seq"`
	AgentID string          `json:"agent_id"`
	Payload json.RawMessage `json:"payload"`
}

// marshalEvents serialises the window as the user message. Events keep their seq
// so the model can attribute each candidate's provenance back to a specific event.
func marshalEvents(events []ext.InputEvent) (string, error) {
	in := make([]wireEventInput, len(events))
	for i, ev := range events {
		payload := ev.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("null")
		}
		in[i] = wireEventInput{Seq: ev.Seq, AgentID: ev.AgentID, Payload: payload}
	}
	b, err := json.Marshal(struct {
		Events []wireEventInput `json:"events"`
	}{Events: in})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type wireResult struct {
	Memories []wireMemory `json:"memories"`
	Claims   []wireClaim  `json:"claims"`
	Entities []wireEntity `json:"entities"`
}

type wireMemory struct {
	Kind      string `json:"kind"`
	Content   string `json:"content"`
	SourceSeq int64  `json:"source_seq"`
}

type wireClaim struct {
	Entity    string          `json:"entity"`
	Predicate string          `json:"predicate"`
	Value     json.RawMessage `json:"value"`
	EventTime *time.Time      `json:"event_time"`
	SourceSeq int64           `json:"source_seq"`
}

type wireEntity struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Aliases []string `json:"aliases"`
}

// toResult maps the model's structured output into ext types. When the window
// holds exactly one event a candidate's provenance is unambiguous, so a missing
// or wrong source_seq is repaired to that event's seq; with multiple events the
// model's source_seq is trusted, and the write path drops any candidate whose seq
// falls outside the window.
func (r wireResult) toResult(events []ext.InputEvent) ext.ExtractResult {
	soleSeq := int64(-1)
	if len(events) == 1 {
		soleSeq = events[0].Seq
	}
	resolve := func(seq int64) int64 {
		if soleSeq >= 0 {
			return soleSeq
		}
		return seq
	}

	var res ext.ExtractResult
	for _, m := range r.Memories {
		// A memory with no content is not a fact worth remembering; drop it rather
		// than persist a blank, provenance-carrying row that recall could surface as
		// an empty result. (Same "sanitise the model's output" discipline as
		// normalizeKind above.)
		if m.Content == "" {
			continue
		}
		res.Memories = append(res.Memories, ext.CandidateMemory{
			Kind:      normalizeKind(m.Kind),
			Content:   m.Content,
			SourceSeq: resolve(m.SourceSeq),
		})
	}
	for _, c := range r.Claims {
		res.Claims = append(res.Claims, ext.CandidateClaim{
			Entity:    c.Entity,
			Predicate: c.Predicate,
			Value:     c.Value,
			EventTime: c.EventTime,
			SourceSeq: resolve(c.SourceSeq),
		})
	}
	for _, e := range r.Entities {
		res.Entities = append(res.Entities, ext.EntityMention{
			Name:    e.Name,
			Type:    e.Type,
			Aliases: e.Aliases,
		})
	}
	return res
}

// normalizeKind coerces the model's kind into the closed vocabulary the memories
// table accepts for extracted memories. An unrecognised kind defaults to
// "semantic" so a model quirk can never abort the persist on the kind CHECK
// constraint. ("working" is reserved for system-promoted hot facts, not
// extraction, so it is not an accepted extraction kind here.)
func normalizeKind(kind string) string {
	switch kind {
	case "semantic", "episodic", "procedural":
		return kind
	default:
		return "semantic"
	}
}

// toolInput returns the JSON input of the first tool_use block naming tool.
func toolInput(msg *anthropicsdk.Message, tool string) (json.RawMessage, bool) {
	if msg == nil {
		return nil, false
	}
	for _, block := range msg.Content {
		if block.Type == "tool_use" && block.Name == tool {
			return block.Input, true
		}
	}
	return nil, false
}

// mapError classifies a Messages API failure. A 4xx the provider returns outright
// (bad request, auth, permission, not found) will not succeed on retry, so it
// surfaces as a plain error. Rate limits (429), server errors (5xx), timeouts, and
// transport failures are transient and surface as ext.ErrExtractorUnavailable so
// the coalesced job retries.
func mapError(err error) error {
	var apiErr *anthropicsdk.Error
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 && apiErr.StatusCode != http.StatusTooManyRequests {
			return fmt.Errorf("extract/anthropic: provider rejected request (status %d): %w", apiErr.StatusCode, err)
		}
	}
	return fmt.Errorf("%w: %w", ext.ErrExtractorUnavailable, err)
}
