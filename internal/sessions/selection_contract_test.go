package sessions

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

type selectionConfiguredModel struct {
	*testkit.FakeModel
	cfg llm.ModelConfig
}

func (m selectionConfiguredModel) Configuration() llm.ModelConfig { return m.cfg }

func selectionTrustedConfig(name string) llm.ModelConfig {
	declared := llm.Capability{Status: llm.Declared, Evidence: []string{"local deterministic selection fixture"}}
	return llm.ModelConfig{Provider: "fixture", Protocol: "openai-chat", Model: name, Version: name + "-v1", Endpoint: "https://example.invalid/v1", NoCredentials: true, AccountScope: "local",
		Capabilities: llm.ModelCapabilities{Items: map[llm.CapabilityName]llm.Capability{llm.CapText: declared, llm.CapTextStream: declared, llm.CapTools: declared, llm.CapContextWindow: declared, llm.CapOutputLimit: declared, llm.CapPhysicalRequestMetering: declared}, ContextWindowTokens: 4096, MaxOutputTokens: 512},
		Parameters:   llm.ModelParameters{MaxOutputTokens: 128, PolicyVersion: "selection-v1"}}
}

func TestP2ModelSwitchInvalidConfigurationPreservesSelection(t *testing.T) {
	for _, applyAt := range []string{"next_turn", "next_trace"} {
		for _, mutation := range []string{"tools", "stream", "output", "context", "thinking"} {
			t.Run(applyAt+"/"+mutation, func(t *testing.T) {
				f, release := runningSelectionFixture(t)
				defer release()
				selected := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "selected"}), selectionTrustedConfig("selected")}
				choose := func(model selectionConfiguredModel, key string) error {
					if applyAt == "next_turn" {
						_, err := f.s.SelectNextTurnModel(t.Context(), SelectNextTurnModelRequest{TraceID: f.input.TraceID, Model: ModelChoice{Model: model}, ExpectedRevision: f.manager.View().LastSeq, IdempotencyKey: key})
						return err
					}
					_, err := f.s.SetDefaultModel(t.Context(), SetDefaultModelRequest{Model: ModelChoice{Model: model}, ExpectedRevision: f.manager.View().LastSeq, IdempotencyKey: key})
					return err
				}
				if err := choose(selected, "accepted"); err != nil {
					t.Fatal(err)
				}
				bad := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not run"}), selectionTrustedConfig("bad")}
				code := product.CodeUnsupportedCapability
				switch mutation {
				case "tools":
					delete(bad.cfg.Capabilities.Items, llm.CapTools)
				case "stream":
					delete(bad.cfg.Capabilities.Items, llm.CapTextStream)
				case "output":
					bad.cfg.Parameters.MaxOutputTokens = 1000
					code = product.CodeInvalidArgument
				case "context":
					bad.cfg.Capabilities.ContextWindowTokens = 0
				case "thinking":
					bad.cfg.Parameters.DefaultThinking = "high"
				}
				before := f.manager.View()
				err := choose(bad, "rejected")
				requireSessionCode(t, err, code)
				if !reflect.DeepEqual(before, f.manager.View()) || selected.Calls() != 0 || bad.Calls() != 0 || f.model.Calls() != 1 || f.runs.Load() != 0 {
					t.Fatal("invalid model mutated selection or invoked work")
				}
				release()
				waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
				if applyAt == "next_trace" {
					input, err := f.s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"independent"}`)})
					if err != nil {
						t.Fatal(err)
					}
					waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[input.TraceID].State) })
				}
				if selected.Calls() != 1 || bad.Calls() != 0 || f.runs.Load() != 1 {
					t.Fatalf("accepted=%d rejected=%d tools=%d", selected.Calls(), bad.Calls(), f.runs.Load())
				}
			})
		}
	}
}

func TestP2SelectionToolCapabilityFailureKeepsPendingInventory(t *testing.T) {
	f, release := runningSelectionFixture(t)
	defer release()
	if _, err := f.s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: f.input.TraceID, ToolNames: []string{}, ExpectedRevision: f.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
	selected := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "text only"}), selectionTrustedConfig("text-only")}
	delete(selected.cfg.Capabilities.Items, llm.CapTools)
	if _, err := f.s.SelectNextTurnModel(t.Context(), SelectNextTurnModelRequest{TraceID: f.input.TraceID, Model: ModelChoice{Model: selected}, ExpectedRevision: f.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
	before := f.manager.View()
	_, err := f.s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: f.input.TraceID, ToolNames: []string{"work"}, ExpectedRevision: before.LastSeq})
	requireSessionCode(t, err, product.CodeUnsupportedCapability)
	if !reflect.DeepEqual(before, f.manager.View()) || selected.Calls() != 0 || f.runs.Load() != 0 {
		t.Fatal("capability failure replaced the accepted selection")
	}
	release()
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	if f.manager.View().Traces[f.input.TraceID].State != "completed" || f.model.Calls() != 1 || selected.Calls() != 1 || f.runs.Load() != 1 {
		t.Fatal("original pending selection was lost")
	}
}

func TestP2ModelSwitchRevalidatesAcceptedIdentityBeforeActivation(t *testing.T) {
	f, release := runningSelectionFixture(t)
	defer release()
	selected := &selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not run"}), selectionTrustedConfig("selected")}
	if _, err := f.s.SelectNextTurnModel(t.Context(), SelectNextTurnModelRequest{TraceID: f.input.TraceID, Model: ModelChoice{Model: selected}, ExpectedRevision: f.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
	selected.cfg.Model = "different-model"
	release()
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	view := f.manager.View()
	if view.Traces[f.input.TraceID].State != "failed" || f.model.Calls() != 1 || selected.Calls() != 0 || f.runs.Load() != 1 {
		t.Fatalf("changed identity executed: trace=%s model=%d selected=%d tools=%d", view.Traces[f.input.TraceID].State, f.model.Calls(), selected.Calls(), f.runs.Load())
	}
	for _, choice := range view.Selections {
		if choice.State != "pending" {
			t.Fatal("identity failure activated the selection")
		}
	}
}

func TestP2SelectionToolsTerminalRetryAfterDiskReopen(t *testing.T) {
	gate := make(chan struct{})
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	model := testkit.NewFake(testkit.Step{Text: "done", Gate: gate})
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: model, GenerationFingerprint: "selection-contract-v1"}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return model.Calls() == 1 })
	request := SetActiveToolsRequest{TraceID: input.TraceID, ToolNames: []string{}, ExpectedRevision: s.rt.manager.View().LastSeq, IdempotencyKey: "tools-terminal"}
	receipt, err := s.SetActiveTools(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	close(gate)
	waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[input.TraceID].State) })
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	if _, ok := reopened.rt.opts.Store.(*jsonl.Store); !ok || reopened.rt.opts.Store == s.rt.opts.Store {
		t.Fatal("did not reopen a fresh disk store")
	}
	before := reopened.rt.manager.View()
	again, err := reopened.SetActiveTools(t.Context(), request)
	if err != nil || again != receipt {
		t.Fatalf("terminal retry=%+v err=%v", again, err)
	}
	conflict := request
	conflict.ToolNames = []string{"unregistered"}
	_, err = reopened.SetActiveTools(t.Context(), conflict)
	requireSessionCode(t, err, product.CodeIdempotencyConflict)
	conflict = request
	conflict.InvocationID = "different-invocation"
	_, err = reopened.SetActiveTools(t.Context(), conflict)
	requireSessionCode(t, err, product.CodeIdempotencyConflict)
	request.IdempotencyKey = "new-terminal-request"
	request.ExpectedRevision = before.LastSeq
	_, err = reopened.SetActiveTools(t.Context(), request)
	requireSessionCode(t, err, product.CodeStateConflict)
	if !reflect.DeepEqual(before, reopened.rt.manager.View()) || model.Calls() != 1 {
		t.Fatal("terminal retry mutated state or executed the model")
	}
}

func TestP2SelectionToolsRejectForeignInvocationAndPreserveSelection(t *testing.T) {
	f, release := runningSelectionFixture(t)
	defer release()
	view := f.manager.View()
	request := SetActiveToolsRequest{TraceID: f.input.TraceID, ToolNames: []string{"work"}, ExpectedRevision: view.LastSeq, IdempotencyKey: "valid-tools"}
	if _, err := f.s.SetActiveTools(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	before := f.manager.View()
	request.InvocationID = "foreign-invocation"
	request.IdempotencyKey = "foreign-tools"
	request.ExpectedRevision = before.LastSeq
	_, err := f.s.SetActiveTools(t.Context(), request)
	requireSessionCode(t, err, product.CodeStateConflict)
	request.InvocationID = ""
	request.IdempotencyKey = "mixed-tools"
	request.ToolNames = []string{"work", "missing"}
	_, err = f.s.SetActiveTools(t.Context(), request)
	requireSessionCode(t, err, product.CodeNotFound)
	if !reflect.DeepEqual(before, f.manager.View()) || f.runs.Load() != 0 || f.model.Calls() != 1 {
		t.Fatal("invalid selection changed state or executed work")
	}
}

func TestP2SelectionToolsNormalizeNamesBeforePersisting(t *testing.T) {
	f, release := runningSelectionFixture(t)
	defer release()
	request := SetActiveToolsRequest{TraceID: f.input.TraceID, ToolNames: []string{" work "}, ExpectedRevision: f.manager.View().LastSeq, IdempotencyKey: "normalized"}
	receipt, err := f.s.SetActiveTools(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	selected, ok := selectionForOperation(f.manager.View(), receipt.OperationID)
	if !ok || !reflect.DeepEqual(selected.ToolNames, []string{"work"}) || !reflect.DeepEqual(selected.ToolVersions, []string{"1"}) {
		t.Fatalf("selection=%+v", selected)
	}
	before := f.manager.View()
	request.ToolNames = []string{"work"}
	again, err := f.s.SetActiveTools(t.Context(), request)
	if err != nil || again != receipt || !reflect.DeepEqual(before, f.manager.View()) {
		t.Fatalf("normalized retry=%+v err=%v", again, err)
	}
}
