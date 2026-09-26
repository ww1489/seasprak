package sessions

import (
	"context"
	"encoding/json"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type versionedPauseModel struct {
	*testkit.FakeModel
}

func (versionedPauseModel) Configuration() llm.ModelConfig {
	return llm.ModelConfig{Version: "model-config-v1"}
}

func TestPauseBindsModelVersionAndOriginalSelection(t *testing.T) {
	backend, err := memory.Open("pause-version", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "pause-version")
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	model := versionedPauseModel{FakeModel: testkit.NewFake(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}})}
	s, err := Start(Options{SessionID: "pause-version", Profile: ProfileMemory, Store: backend, Model: model,
		Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
			t.Error("tool ran past model pause")
			return "", nil
		}}}, ToolInfos: []*schema.ToolInfo{testkit.ToolInfo("work", "work")}}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	first, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for model.Calls() == 0 {
		select {
		case <-deadline:
			t.Fatal("model never started")
		default:
			goruntime.Gosched()
		}
	}
	// PrepareNextTurn fixes the selection at the revision immediately before
	// BeginTurnID persists the first logical model call for this turn.
	stored, err := backend.Load(t.Context(), "pause-version")
	if err != nil {
		t.Fatal(err)
	}
	var originalSelection uint64
	for _, commit := range stored.Commits {
		for _, record := range commit.ControlRecords {
			if record.Type != "trace" {
				continue
			}
			var trace state.TraceState
			if err := json.Unmarshal(record.Payload, &trace); err != nil {
				t.Fatal(err)
			}
			if originalSelection == 0 && trace.ID == first.TraceID && trace.Usage.ModelCallID != "" {
				originalSelection = commit.ExpectedPreviousSeq
			}
		}
	}
	if originalSelection == 0 {
		t.Fatal("turn has no selected history revision")
	}
	result := make(chan error, 1)
	go func() { _, err := s.Pause(context.Background(), first.TraceID); result <- err }()
	for len(manager.View().Operations) == 0 {
		select {
		case <-deadline:
			t.Fatal("pause not accepted")
		default:
			goruntime.Gosched()
		}
	}
	if err := s.rt.do(t.Context(), func(*runtime) error { return nil }); err != nil {
		t.Fatal(err)
	}
	close(gate)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not settle")
	}
	view := manager.View()
	if len(view.Checkpoints) != 1 || len(view.ModelAttempts) != 1 || model.Calls() != 1 {
		t.Fatalf("checkpoint=%d attempt=%d calls=%d", len(view.Checkpoints), len(view.ModelAttempts), model.Calls())
	}
	for _, cp := range view.Checkpoints {
		for _, attempt := range view.ModelAttempts {
			if cp.Scope.TurnID != attempt.Scope.TurnID || cp.ModelConfigVersion != attempt.ModelConfigVersion || cp.ModelConfigVersion != "model-config-v1" || cp.SelectionRevision != originalSelection || cp.SelectionRevision >= view.LastSeq {
				t.Fatalf("checkpoint lost original model or selection: cp=%+v attempt=%+v selection=%d", cp, attempt, originalSelection)
			}
		}
	}
}
