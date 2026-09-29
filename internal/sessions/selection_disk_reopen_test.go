package sessions

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2ModelSwitchDefaultSelectionSurvivesDiskReopen(t *testing.T) {
	original := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "original"}), selectionTrustedConfig("A")}
	selected := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "selected"}), selectionTrustedConfig("B")}
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: original, GenerationFingerprint: "selection-disk-v1"}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	receipt, err := s.SetDefaultModel(t.Context(), SetDefaultModelRequest{Model: ModelChoice{Model: selected}, ExpectedRevision: s.rt.manager.View().LastSeq, IdempotencyKey: "default-B"})
	if err != nil {
		t.Fatal(err)
	}
	selection, ok := selectionForOperation(s.rt.manager.View(), receipt.OperationID)
	if !ok {
		t.Fatal("default selection not committed")
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The caller supplies the trusted current implementation again. Disk state
	// only supplies its stable identity, never executable model configuration.
	rebuilt := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "rebuilt B"}), selectionTrustedConfig("B")}
	opts.Model = rebuilt
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	if _, ok := opened.rt.opts.Store.(*jsonl.Store); !ok || opened.rt.opts.Store == s.rt.opts.Store {
		t.Fatal("expected fresh JSONL-backed session")
	}
	if original.Calls() != 0 || selected.Calls() != 0 || rebuilt.Calls() != 0 {
		t.Fatal("opening executed a model")
	}
	input, err := opened.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"new trace"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(opened.rt.manager.View().Traces[input.TraceID].State) })
	view := opened.rt.manager.View()
	trace := view.Traces[input.TraceID]
	if trace.ModelSelectionID != selection.ID || trace.State != "completed" || rebuilt.Calls() != 1 || original.Calls() != 0 || selected.Calls() != 0 {
		t.Fatalf("reopened default trace=%+v calls A=%d oldB=%d newB=%d", trace, original.Calls(), selected.Calls(), rebuilt.Calls())
	}
}

func TestP2SelectionPendingApprovalPreservesCheckpointAfterDiskReopen(t *testing.T) {
	for _, kind := range []string{"tools", "model"} {
		t.Run(kind, func(t *testing.T) {
			original := selectionConfiguredModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "approved", Name: "work", Arguments: `{}`}}}), selectionTrustedConfig("A")}
			var runs atomic.Int32
			opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Principal: "operator", Model: original, GenerationFingerprint: "selection-disk-v1", Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "approved", nil }}}}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"approval"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool {
				trace := s.rt.manager.View().Traces[input.TraceID]
				return trace.State == "paused" || terminal(trace.State)
			})
			before := s.rt.manager.View()
			cp := before.Checkpoints[before.Traces[input.TraceID].CheckpointID]
			if len(cp.CallIDs) != 1 || original.Calls() != 1 || runs.Load() != 0 {
				t.Fatalf("approval checkpoint=%+v model=%d tools=%d", cp, original.Calls(), runs.Load())
			}
			if kind == "tools" {
				_, err = s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: input.TraceID, ToolNames: []string{}, ExpectedRevision: before.LastSeq, IdempotencyKey: "pending-tools"})
			} else {
				_, err = s.SelectNextTurnModel(t.Context(), SelectNextTurnModelRequest{TraceID: input.TraceID, Model: ModelChoice{Model: original}, ExpectedRevision: before.LastSeq, IdempotencyKey: "pending-model"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == "tools" {
				_, err = s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: input.TraceID, ToolNames: []string{}, ExpectedRevision: s.rt.manager.View().LastSeq, IdempotencyKey: "latest-tools"})
			} else {
				_, err = s.SelectNextTurnModel(t.Context(), SelectNextTurnModelRequest{TraceID: input.TraceID, Model: ModelChoice{Model: original}, ExpectedRevision: s.rt.manager.View().LastSeq, IdempotencyKey: "latest-model"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cp, s.rt.manager.View().Checkpoints[cp.ID]) || !reflect.DeepEqual(before.Turns[cp.Scope.TurnID], s.rt.manager.View().Turns[cp.Scope.TurnID]) {
				t.Fatal("pending choice modified frozen checkpoint or turn")
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			rebuilt := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "resumed"}), selectionTrustedConfig("A")}
			opts.Model = rebuilt
			opened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = opened.Close(context.Background()) })
			if _, ok := opened.rt.opts.Store.(*jsonl.Store); !ok || opened.rt.opts.Store == s.rt.opts.Store {
				t.Fatal("expected fresh JSONL-backed session")
			}
			if rebuilt.Calls() != 0 || runs.Load() != 0 {
				t.Fatal("opening resumed execution")
			}
			fresh := resumeForFreshApproval(t, opened, input.TraceID)
			if rebuilt.Calls() != 0 || runs.Load() != 0 {
				t.Fatal("unanswered Resume executed work")
			}
			for id := range fresh.Interactions {
				if _, err := opened.RespondInteraction(t.Context(), InteractionResponse{InteractionID: id, Decision: "allowed-once", ExpectedRevision: opened.rt.manager.View().LastSeq}); err != nil {
					t.Fatal(err)
				}
			}
			receipt, err := opened.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: opened.rt.manager.View().LastSeq})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(opened.rt.manager.View().Traces[input.TraceID].State) })
			view := opened.rt.manager.View()
			op, err := opened.rt.manager.GetOperation(receipt.OperationID)
			if err != nil || op.State != "completed" || view.Traces[input.TraceID].State != "completed" || original.Calls() != 1 || rebuilt.Calls() != 1 || runs.Load() != 1 {
				t.Fatalf("resume=%+v err=%v model=%d reopened=%d tools=%d trace=%+v", op, err, original.Calls(), rebuilt.Calls(), runs.Load(), view.Traces[input.TraceID])
			}
			call := view.Calls[cp.CallIDs[0]]
			if call.Scope.TurnID != cp.Scope.TurnID || call.Call.ProviderCallID != "approved" || call.Observation == nil || len(view.Calls) != 1 || !reflect.DeepEqual(view.Turns[cp.Scope.TurnID].ToolNames, before.Turns[cp.Scope.TurnID].ToolNames) || !reflect.DeepEqual(cp, view.Checkpoints[cp.ID]) {
				t.Fatal("resume changed original call, inventory, or checkpoint")
			}
			for id, turn := range view.Turns {
				if id != cp.Scope.TurnID && kind == "tools" && len(turn.ToolNames) != 0 {
					t.Fatal("next-turn inventory was not activated")
				}
			}
		})
	}
}
