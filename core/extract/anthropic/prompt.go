package anthropic

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"sync"
	"time"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/lore-gpt/lore/core/ext"
)

// PromptDigest names what this extractor asks the model without publishing it: the first twelve hex
// characters of a SHA-256 over the system prompt, the forced tool's definition, and a fixed probe window
// rendered exactly as a real request renders its events. Changing the instructions, the tool's schema or
// descriptions, or the way a window of events is laid out for the model moves the digest; nothing else does.
//
// The model and its settings are named elsewhere. This covers what they cannot: two builds on the same model
// with different instructions distil different memories, and anyone reading a deployment's identity — an
// operator comparing two stacks, or a measurement deciding whether a resumed run may continue — can now tell
// those builds apart instead of filing both under one name.
//
// It is empty only if this package's own values fail to encode, which a test rules out; a caller treats an
// empty digest as a prompt it cannot name.
func PromptDigest() string {
	digest, err := promptDigest()
	if err != nil {
		return ""
	}
	return digest
}

var promptDigest = sync.OnceValues(func() (string, error) {
	return digestOf(systemPrompt, extractionTool, probeWindow)
})

// digestOf is PromptDigest over explicit inputs, so a test can show that each of them moves the result.
func digestOf(system string, tool anthropicsdk.ToolParam, window []ext.InputEvent) (string, error) {
	toolJSON, err := json.Marshal(tool)
	if err != nil {
		return "", fmt.Errorf("extract/anthropic: encode tool for prompt digest: %w", err)
	}
	events, err := marshalEvents(window)
	if err != nil {
		return "", fmt.Errorf("extract/anthropic: encode probe window for prompt digest: %w", err)
	}
	h := sha256.New()
	for _, part := range []string{system, string(toolJSON), events} {
		writePart(h, part)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// writePart writes one length-prefixed part, so text moving across the boundary between two parts cannot
// leave the digest unchanged.
func writePart(h hash.Hash, part string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(part)))
	h.Write(n[:])
	h.Write([]byte(part))
}

// probeWindow is rendered through the same encoder a real request uses, so a change to how events are laid
// out for the model — a field added, renamed or dropped — moves the digest even when no instruction changed.
// It sets every field of ext.InputEvent: a field left at its zero value could be encoded differently, or not
// at all, without this rendering noticing, so a test fails when a new field is left unset here.
var probeWindow = []ext.InputEvent{{
	Seq:        1,
	AgentID:    "probe",
	OccurredAt: time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC),
	Payload:    json.RawMessage(`{"content":"probe"}`),
}}
