package codeagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestApprovalWaitsForDurableCheckpointWithoutExecuting(t *testing.T) {
	for _, kind := range []string{"invokable", "enhanced-invokable"} {
		t.Run(kind, func(t *testing.T) {
			id := agent.MustID()
			backend, err := memory.Open(id, store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			manager, err := state.NewManager(backend, id)
			if err != nil {
				t.Fatal(err)
			}
			model := versionedPauseModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "finished"})}
			var runs atomic.Int32
			opts := Options{SessionID: id, Workspace: t.TempDir(), Principal: "host-user", Profile: ProfileMemory, GenerationFingerprint: "approval-bundle-v1", Store: backend, Model: model, Tools: []tools.Definition{{Name: "work", Version: "1", ToolInterface: kind, Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, Run: func(_ context.Context, _ json.RawMessage) (string, error) {
				runs.Add(1)
				return "approved result", nil
			}}}}
			if _, err := alignTools(&opts); err != nil {
				t.Fatal(err)
			}
			s, err := Start(opts, manager, "gen")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool {
				tr := manager.View().Traces[input.TraceID]
				return tr.State == "paused" || terminal(tr.State)
			})
			v := manager.View()
			tr := v.Traces[input.TraceID]
			if tr.State != "paused" || tr.Settled || !tr.ExecutionStopped || tr.CheckpointID == "" || len(v.Interactions) != 0 || len(v.Approvals) != 0 || len(approvalSnapshot(t, s).Interactions) != 1 || model.Calls() != 1 || runs.Load() != 0 || tr.Usage.ToolExecutions != 0 {
				t.Fatalf("approval did not safely wait: trace=%+v interactions=%d approvals=%d model=%d runs=%d", tr, len(v.Interactions), len(v.Approvals), model.Calls(), runs.Load())
			}
			for _, call := range v.Calls {
				if call.Claimed || call.Observation != nil || v.Turns[call.Scope.TurnID].Ended {
					t.Fatalf("waiting approval manufactured a result: %+v", call)
				}
			}
			for _, in := range approvalSnapshot(t, s).Interactions {
				cmd := InteractionResponse{InteractionID: in.ID, Decision: "allowed-once", ExpectedRevision: manager.View().LastSeq}
				if _, err := s.RespondInteraction(t.Context(), cmd); err != nil {
					t.Fatal(err)
				}
			}
			if runs.Load() != 0 || model.Calls() != 1 || manager.View().Traces[input.TraceID].State != "paused" {
				t.Fatal("decision automatically executed work")
			}
			_, err = s.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: manager.View().LastSeq})
			if err != nil {
				t.Fatalf("approved checkpoint cannot explicitly resume: %v", err)
			}
			waitResumeCondition(t, func() bool { return terminal(manager.View().Traces[input.TraceID].State) })
			resumed := manager.View()
			tr = resumed.Traces[input.TraceID]
			if tr.ExecutionID == "" || tr.ExecutionID == v.Traces[input.TraceID].ExecutionID || len(v.Calls) != 1 || len(resumed.Calls) != 1 {
				t.Fatalf("resume did not retain one call in a new execution segment: before=%+v after=%+v", v.Traces[input.TraceID], tr)
			}
			for id, original := range v.Calls {
				current := resumed.Calls[id]
				if current.Scope != original.Scope || current.Call != original.Call || !current.Claimed || current.Observation == nil || !current.Observation.Executed || current.Observation.Status != "succeeded" {
					t.Fatalf("resumed selection check lost the original call/Turn or result: before=%+v after=%+v", original, current)
				}
			}
			if tr.State != "completed" || runs.Load() != 1 || model.Calls() != 2 || tr.Usage.ToolExecutions != 1 {
				t.Fatalf("approved execution changed counts: trace=%+v runs=%d models=%d", tr, runs.Load(), model.Calls())
			}
		})
	}
}
