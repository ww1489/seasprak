package eino

import (
	"bytes"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/testkit"
)

func checkpointExtraTree() map[string]any {
	return map[string]any{"nested": map[string]any{"list": []any{"synthetic-private-extra", true, float64(1.25), nil, map[string]any{"leaf": "kept"}}}, "number": json.Number("9007199254740993"), "raw": json.RawMessage(`{"n":9007199254740993}`), "bytes": []byte{0, 1, 255}}
}

func decorateCheckpointExtra(msg *schema.AgenticMessage, value any) {
	msg.Extra["provider"] = value
	msg.ContentBlocks[0].Extra = map[string]any{"provider": value}
	msg.ResponseMeta = &schema.AgenticResponseMeta{Extension: value}
	msg.ContentBlocks = append(msg.ContentBlocks, schema.NewContentBlock(&schema.AssistantGenText{Text: "synthetic", Extension: value}))
}

func assertCheckpointExtra(t *testing.T, in []*schema.AgenticMessage, want any) {
	t.Helper()
	// Eino v0.9.21's internal state copy reconstructs named slices with
	// reflect.SliceOf. RawMessage therefore keeps its bytes, not its named type.
	// This is a bounded compatibility assertion, not end-to-end RawMessage support.
	if tree, ok := want.(map[string]any); ok {
		if raw, ok := tree["raw"].(json.RawMessage); ok {
			copy := make(map[string]any, len(tree))
			for key, value := range tree {
				copy[key] = value
			}
			copy["raw"] = []byte(raw)
			want = copy
		}
	}
	for _, msg := range in {
		if msg.Role != schema.AgenticRoleTypeAssistant {
			continue
		}
		if len(msg.ContentBlocks) < 2 || msg.ResponseMeta == nil {
			t.Error("checkpoint lost assistant metadata")
			return
		}
		got := []any{msg.Extra["provider"], msg.ContentBlocks[0].Extra["provider"], msg.ResponseMeta.Extension, msg.ContentBlocks[len(msg.ContentBlocks)-1].AssistantGenText.Extension}
		for i, value := range got {
			if !reflect.DeepEqual(value, want) {
				if tree, ok := value.(map[string]any); ok {
					t.Logf("checkpoint location %d concrete types: number=%T (%v), raw=%T, bytes=%T", i, tree["number"], tree["number"], tree["raw"], tree["bytes"])
				}
				t.Errorf("checkpoint location %d: got %#v want %#v", i, value, want)
			}
		}
		return
	}
	t.Error("resumed model did not receive checkpointed assistant")
}

// The gob boundary itself must preserve concrete interface types. The real
// TurnLoop test below additionally covers Eino's internal state-copy boundary.
func TestCheckpointExtraGobTypes(t *testing.T) {
	want := checkpointExtraTree()
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(want); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gob changed concrete types or values: got %#v want %#v", got, want)
	}
	t.Logf("gob preserves number=%T (%v), raw=%T, bytes=%T", got["number"], got["number"], got["raw"], got["bytes"])
}

func TestCheckpointLegacyScalarResume(t *testing.T) {
	encoded, err := os.ReadFile("testdata/checkpoint_scalar_v0.9.21.b64")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePausedCheckpoint(blob, checkpointPrompt); err != nil {
		t.Fatal(err)
	}
	h := newCheckpointHarness(true)
	t.Cleanup(h.closeRelease)
	if err := h.store.Set(t.Context(), h.cpID, blob); err != nil {
		t.Fatal(err)
	}
	h.fake = testkit.NewFake(testkit.Step{Text: "legacy resumed"})
	h.model.inner = h.fake
	h.model.inspect = func(in []*schema.AgenticMessage) { assertCheckpointExtra(t, in, "synthetic-legacy-scalar") }
	h.resumed.Store(true)
	resume, cancel := h.startLoop(t, true, "turn-1")
	if exit := h.waitLoop(t, resume, cancel); exit.ExitReason != nil {
		t.Fatal(exit.ExitReason)
	}
	if h.genInput.Load() != 0 || h.genResume.Load() != 1 || h.fake.Calls() != 1 || h.tool.total() != 1 || !h.model.sawToolResult.Load() {
		t.Fatalf("legacy resume input=%d resume=%d model=%d tools=%d result=%v", h.genInput.Load(), h.genResume.Load(), h.fake.Calls(), h.tool.total(), h.model.sawToolResult.Load())
	}
}

func TestCheckpointExtraResume(t *testing.T) {

	for caseName, value := range map[string]any{"nested": map[string]any{"tree": []any{"synthetic-private-extra", map[string]any{"leaf": true}}}, "types": checkpointExtraTree()} {
		t.Run(caseName, func(t *testing.T) {
			for _, gateModel := range []bool{true, false} {
				name := "after-model"
				if !gateModel {
					name = "after-tool"
				}
				t.Run(name, func(t *testing.T) {
					h := newCheckpointHarness(gateModel)
					t.Cleanup(h.closeRelease)
					h.model.decorate = func(msg *schema.AgenticMessage) { decorateCheckpointExtra(msg, value) }
					loop, cancel := h.startLoop(t, false, "")
					if ok, _ := loop.Push(checkpointPrompt); !ok {
						t.Fatal("Push rejected")
					}
					signal := h.model.started
					if !gateModel {
						signal = h.tool.started
					}
					h.waitSignal(t, loop, cancel, signal, "safe point not entered")
					loop.Stop(adk.WithGraceful())
					exit := h.waitLoop(t, loop, cancel)
					assertCancelCheckpoint(t, exit, h.store, h.cpID)
					beforeTools := int32(0)
					if !gateModel {
						beforeTools = 1
					}
					if h.tool.total() != beforeTools {
						t.Fatalf("pre-resume tools=%d", h.tool.total())
					}
					h.rebuildBudget()
					h.resumed.Store(true)
					h.model.decorate = nil
					h.model.inspect = func(in []*schema.AgenticMessage) { assertCheckpointExtra(t, in, value) }
					resume, resumeCancel := h.startLoop(t, true, "turn-1")
					if exit := h.waitLoop(t, resume, resumeCancel); exit.ExitReason != nil {
						t.Fatal(exit.ExitReason)
					}
					if h.genResume.Load() != 1 || h.genInput.Load() != 1 || h.fake.Calls() != 2 || h.tool.total() != 1 || !h.model.sawToolResult.Load() {
						t.Fatalf("resume calls input=%d resume=%d model=%d tool=%d result=%v", h.genInput.Load(), h.genResume.Load(), h.fake.Calls(), h.tool.total(), h.model.sawToolResult.Load())
					}
					assertUsage(t, h.budget, 2, 2, 1)
				})
			}
		})
	}
}
