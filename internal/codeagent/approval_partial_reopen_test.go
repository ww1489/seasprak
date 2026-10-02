package codeagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestApprovalDiskReopenReusesCompletedCallAndReasksRemaining(t *testing.T) {
	var runs [2]atomic.Int32
	model := versionedPauseModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "first", Name: "work", Arguments: `{"index":0}`}, {CallID: "second", Name: "work", Arguments: `{"index":1}`}}}, testkit.Step{Text: "done"})}
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Principal: "host", GenerationFingerprint: "approval-partial-reopen", Model: model, Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, Run: func(_ context.Context, args json.RawMessage) (string, error) {
		var input struct{ Index int }
		if err := json.Unmarshal(args, &input); err != nil {
			return "", err
		}
		runs[input.Index].Add(1)
		return "done", nil
	}}}}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"work"}`)})
	if err != nil {
		t.Fatal(err)
	}
	wait := func(session *AgentSession) {
		t.Helper()
		waitResumeCondition(t, func() bool {
			tr := session.rt.manager.View().Traces[input.TraceID]
			return tr.State == "paused" || terminal(tr.State)
		})
	}
	wait(s)
	snap := approvalSnapshot(t, s)
	var remainingID string
	for id, in := range snap.Interactions {
		if snap.Calls[in.CallID].Call.ProviderCallID == "first" {
			if _, err := s.RespondInteraction(t.Context(), InteractionResponse{InteractionID: id, Decision: "allowed-once", ExpectedRevision: snap.Revision}); err != nil {
				t.Fatal(err)
			}
		} else {
			remainingID = id
		}
	}
	if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: snap.Revision}); err != nil {
		t.Fatal(err)
	}
	wait(s)
	before := s.rt.manager.View()
	if before.Traces[input.TraceID].State != "paused" || runs[0].Load() != 1 || runs[1].Load() != 0 || before.Traces[input.TraceID].Usage.ToolExecutions != 1 {
		t.Fatal("partial checkpoint did not retain exactly one result")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	fresh := approvalSnapshot(t, opened)
	if len(fresh.Interactions) != 0 || len(fresh.Approvals) != 0 || fresh.Revision != before.LastSeq || model.Calls() != 1 || runs[0].Load() != 1 || runs[1].Load() != 0 {
		t.Fatal("Open asked, wrote or executed before explicit Resume")
	}
	if _, err := opened.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: fresh.Revision}); err != nil {
		t.Fatal(err)
	}
	wait(opened)
	fresh = approvalSnapshot(t, opened)
	if fresh.Traces[input.TraceID].State != "paused" || fresh.Traces[input.TraceID].Usage.ToolExecutions != 1 || len(fresh.Interactions) != 1 || fresh.Interactions[remainingID].ID != "" || model.Calls() != 1 || runs[0].Load() != 1 || runs[1].Load() != 0 {
		t.Fatal("reopen restored old permission or replayed completed work")
	}
	for id, in := range fresh.Interactions {
		if fresh.Calls[in.CallID].Call.ProviderCallID != "second" {
			t.Fatal("completed call was asked again")
		}
		if _, err := opened.RespondInteraction(t.Context(), InteractionResponse{InteractionID: id, Decision: "allowed-once", ExpectedRevision: fresh.Revision}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := opened.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: fresh.Revision}); err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(opened.rt.manager.View().Traces[input.TraceID].State) })
	after := opened.rt.manager.View()
	if after.Traces[input.TraceID].State != "completed" || runs[0].Load() != 1 || runs[1].Load() != 1 || model.Calls() != 2 || after.Traces[input.TraceID].Usage.ToolExecutions != 2 || len(after.Calls) != 2 {
		t.Fatalf("reopened partial execution duplicated a call: %+v", after.Traces[input.TraceID])
	}
	for id, old := range before.Calls {
		current := after.Calls[id]
		if old.Call != current.Call || old.Scope != current.Scope || current.Observation == nil {
			t.Fatal("original call or observation was lost")
		}
	}
}
