package anthropic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"regexp"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/lore-gpt/lore/core/ext"
)

func TestPromptDigestIsAShortStableName(t *testing.T) {
	got := PromptDigest()
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(got) {
		t.Fatalf("PromptDigest() = %q, want twelve lowercase hex characters", got)
	}
	if again := PromptDigest(); again != got {
		t.Errorf("PromptDigest() changed between calls: %q then %q", got, again)
	}
	want, err := digestOf(systemPrompt, extractionTool, probeWindow)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("PromptDigest() = %q, but the digest of the package's own prompt is %q", got, want)
	}
}

// TestPromptDigestMovesWithEachInput is the point of the digest: an edit to anything the model reads must
// produce a different name, or two builds that distil differently would still report as one.
func TestPromptDigestMovesWithEachInput(t *testing.T) {
	base, err := digestOf(systemPrompt, extractionTool, probeWindow)
	if err != nil {
		t.Fatal(err)
	}
	if same, _ := digestOf(systemPrompt, extractionTool, probeWindow); same != base {
		t.Fatalf("identical inputs gave %q and %q", base, same)
	}

	renamed := extractionTool
	renamed.Name = "record_something_else"
	redescribed := extractionTool
	redescribed.Description = param.NewOpt("Record whatever seems useful.")
	window := func(edit func(*ext.InputEvent)) []ext.InputEvent {
		ev := probeWindow[0]
		edit(&ev)
		return []ext.InputEvent{ev}
	}

	for _, tc := range []struct {
		name string
		got  func() (string, error)
	}{
		{"one character added to the instructions", func() (string, error) {
			return digestOf(systemPrompt+".", extractionTool, probeWindow)
		}},
		{"the tool renamed", func() (string, error) { return digestOf(systemPrompt, renamed, probeWindow) }},
		{"the tool described differently", func() (string, error) {
			return digestOf(systemPrompt, redescribed, probeWindow)
		}},
		{"the probe window's seq changed", func() (string, error) {
			return digestOf(systemPrompt, extractionTool, window(func(ev *ext.InputEvent) { ev.Seq = 2 }))
		}},
		{"the probe window's agent changed", func() (string, error) {
			return digestOf(systemPrompt, extractionTool, window(func(ev *ext.InputEvent) { ev.AgentID = "other" }))
		}},
		{"the probe window's payload changed", func() (string, error) {
			return digestOf(systemPrompt, extractionTool, window(func(ev *ext.InputEvent) {
				ev.Payload = json.RawMessage(`{"text":"probe"}`)
			}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.got()
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Errorf("digest stayed %q", base)
			}
		})
	}
}

// TestPromptDigestPartsCannotTradeText pins the length prefix: without it, text moving from the end of one
// part to the start of the next would hash to the same digest.
func TestPromptDigestPartsCannotTradeText(t *testing.T) {
	sum := func(parts ...string) string {
		h := sha256.New()
		for _, part := range parts {
			writePart(h, part)
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	if sum("ab", "c") == sum("a", "bc") {
		t.Error("two parts that trade a character hash the same")
	}
}

// TestProbeWindowSetsEveryInputEventField keeps the probe honest as the event grows. A field the probe leaves
// at its zero value could be added to the wire — or encoded differently — without the probe's rendering
// changing, and the digest would then miss a change to what the model reads.
func TestProbeWindowSetsEveryInputEventField(t *testing.T) {
	ev := reflect.ValueOf(probeWindow[0])
	for i := range ev.NumField() {
		if ev.Field(i).IsZero() {
			t.Errorf("probeWindow leaves ext.InputEvent.%s unset; give it a non-zero value so the prompt digest "+
				"sees how the field is laid out for the model", ev.Type().Field(i).Name)
		}
	}
}

// TestPromptDigestCoversWhatTheRequestSends is the anti-drift lock between the digest and the request. Both
// read the same package values today; if the request ever took its instructions, tool or event layout from
// somewhere else, the digest would go on naming a prompt nobody sends.
func TestPromptDigestCoversWhatTheRequestSends(t *testing.T) {
	parts, err := buildParts(ext.ExtractInput{Events: probeWindow})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts.system) != 1 || parts.system[0].Text != systemPrompt {
		t.Error("the request's system block is not the prompt the digest covers")
	}
	if len(parts.tools) != 1 || parts.tools[0].OfTool != &extractionTool {
		t.Error("the request's tool is not the definition the digest covers")
	}
	events, err := marshalEvents(probeWindow)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts.messages) != 1 || len(parts.messages[0].Content) != 1 ||
		parts.messages[0].Content[0].OfText == nil || parts.messages[0].Content[0].OfText.Text != events {
		t.Error("the request lays out its events differently from the rendering the digest covers")
	}
}
