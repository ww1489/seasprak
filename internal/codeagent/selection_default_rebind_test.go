package codeagent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2ModelSwitchDefaultDiskRebindPreservesReceipt(t *testing.T) {
	a := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not run A"}), selectionTrustedConfig("A")}
	b := selectionConfiguredModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "approved", Name: "work", Arguments: `{}`}}}), selectionTrustedConfig("B")}
	var runs atomic.Int32
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Principal: "operator", Model: a, GenerationFingerprint: "selection-default-rebind-v1", Tools: []tools.Definition{
		{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "approved", nil }},
	}}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	request := SetDefaultModelRequest{Model: ModelChoice{Model: b}, ExpectedRevision: s.rt.manager.View().LastSeq, IdempotencyKey: "default-B"}
	receipt, err := s.SetDefaultModel(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"use default B"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool {
		v := s.rt.manager.View()
		return v.Traces[input.TraceID].State == "paused" || terminal(v.Traces[input.TraceID].State)
	})
	view := s.rt.manager.View()
	cp := view.Checkpoints[view.Traces[input.TraceID].CheckpointID]
	selected, ok := selectionForOperation(view, receipt.OperationID)
	if !ok || selected.ApplyAt != "next_trace" || view.Traces[input.TraceID].ModelSelectionID != selected.ID || view.Traces[input.TraceID].State != "paused" || cp.ModelConfigVersion != "B-v1" || a.Calls() != 0 || b.Calls() != 1 || runs.Load() != 0 {
		t.Fatal("default B did not actually activate and pause")
	}
	c := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not run C"}), selectionTrustedConfig("C")}
	newer, err := s.SetDefaultModel(t.Context(), SetDefaultModelRequest{Model: ModelChoice{Model: c}, ExpectedRevision: view.LastSeq})
	if err != nil {
		t.Fatal(err)
	}
	latest, ok := selectionForOperation(s.rt.manager.View(), newer.OperationID)
	if !ok || latest.ID == "" || latest.Revision <= selected.Revision {
		t.Fatal("missing newer default")
	}
	if err = s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	rebuiltA := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not run rebuilt A"}), selectionTrustedConfig("A")}
	rebuiltB := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "resumed B"}), selectionTrustedConfig("B")}
	opts.Model = rebuiltA
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	if _, ok := opened.rt.opts.Store.(*jsonl.Store); !ok || opened.rt.opts.Store == s.rt.opts.Store || opened.rt.manager == s.rt.manager {
		t.Fatal("did not reopen a fresh JSONL store and manager")
	}
	if snap := approvalSnapshot(t, opened); len(snap.Interactions) != 0 || len(snap.Approvals) != 0 {
		t.Fatal("Open created pending approval before Resume")
	}
	before := opened.rt.manager.View()
	_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: before.LastSeq})
	requireSessionCode(t, err, product.CodeIncompatibleResume)
	for _, mutation := range []string{"stream", "output"} {
		t.Run(mutation, func(t *testing.T) {
			bad := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not run bad B"}), selectionTrustedConfig("B")}
			if mutation == "stream" {
				delete(bad.cfg.Capabilities.Items, llm.CapTextStream)
			} else {
				bad.cfg.Parameters.MaxOutputTokens = 1000
			}
			request.Model.Model = bad
			again, err := opened.SetDefaultModel(t.Context(), request)
			if err != nil || again != receipt || !reflect.DeepEqual(before, opened.rt.manager.View()) {
				t.Fatalf("invalid default replay changed receipt: %+v %v", again, err)
			}
			assertReboundSelection(t, opened, selected.ID, latest.ID, nil)
			_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: before.LastSeq})
			requireSessionCode(t, err, product.CodeIncompatibleResume)
			if !reflect.DeepEqual(before, opened.rt.manager.View()) || bad.Calls() != 0 || rebuiltA.Calls() != 0 || rebuiltB.Calls() != 0 || c.Calls() != 0 || runs.Load() != 0 {
				t.Fatal("invalid default replay or resume executed")
			}
		})
	}
	request.Model.Model = rebuiltB
	again, err := opened.SetDefaultModel(t.Context(), request)
	if err != nil || again != receipt || !reflect.DeepEqual(before, opened.rt.manager.View()) {
		t.Fatalf("valid default replay changed receipt: %+v %v", again, err)
	}
	assertReboundSelection(t, opened, selected.ID, latest.ID, rebuiltB.FakeModel)
	replacement := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not replace B"}), selectionTrustedConfig("B")}
	request.Model.Model = replacement
	again, err = opened.SetDefaultModel(t.Context(), request)
	if err != nil || again != receipt || !reflect.DeepEqual(before, opened.rt.manager.View()) || rebuiltA.Calls() != 0 || rebuiltB.Calls() != 0 || runs.Load() != 0 {
		t.Fatalf("default replay committed or executed: %+v %v", again, err)
	}
	assertReboundSelection(t, opened, selected.ID, latest.ID, rebuiltB.FakeModel)
	resumeForFreshApproval(t, opened, input.TraceID)
	if rebuiltA.Calls() != 0 || rebuiltB.Calls() != 0 || replacement.Calls() != 0 || runs.Load() != 0 {
		t.Fatal("unanswered Resume replayed model or tool")
	}
	answerCommand(t, opened, "allowed-once")
	resumeStarted := time.Now()
	_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: opened.rt.manager.View().LastSeq})
	if err != nil {
		logSelectionRebindFailure(t, opened, input.TraceID, resumeStarted)
		t.Fatalf("resume rejected: errorCodes=%v checkpointApprovalExpired=%t", selectionRebindErrorCodes(err.Error()), strings.Contains(err.Error(), "checkpoint approval is expired"))
	}
	waitResumeCondition(t, func() bool { return terminal(opened.rt.manager.View().Traces[input.TraceID].State) })
	after := opened.rt.manager.View()
	usage := after.Traces[input.TraceID].Usage
	if after.Traces[input.TraceID].State != "completed" || a.Calls() != 0 || b.Calls() != 1 || rebuiltA.Calls() != 0 || rebuiltB.Calls() != 1 || replacement.Calls() != 0 || c.Calls() != 0 || runs.Load() != 1 || usage.LogicalModelCalls != 2 || usage.TransportRequests != 2 || usage.ToolExecutions != 1 || len(after.Calls) != 1 || !reflect.DeepEqual(cp, after.Checkpoints[cp.ID]) {
		logSelectionRebindFailure(t, opened, input.TraceID, resumeStarted)
		t.Fatalf("default B resume lost identity or counts: state=%s A=%d B=%d rebuiltA=%d rebuiltB=%d replacement=%d C=%d work=%d logical=%d physical=%d tools=%d calls=%d checkpointEqual=%t", after.Traces[input.TraceID].State, a.Calls(), b.Calls(), rebuiltA.Calls(), rebuiltB.Calls(), replacement.Calls(), c.Calls(), runs.Load(), usage.LogicalModelCalls, usage.TransportRequests, usage.ToolExecutions, len(after.Calls), reflect.DeepEqual(cp, after.Checkpoints[cp.ID]))
	}
	for id, original := range view.Calls {
		call := after.Calls[id]
		if call.Scope != original.Scope || call.Call != original.Call || call.Observation == nil {
			t.Fatal("default resume changed full Scope or FrozenCall")
		}
	}
	if err = opened.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	terminalSession, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminalSession.Close(context.Background()) })
	terminalBefore := terminalSession.rt.manager.View()
	bad := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not run terminal"}), selectionTrustedConfig("B")}
	delete(bad.cfg.Capabilities.Items, llm.CapTextStream)
	request.Model.Model = bad
	again, err = terminalSession.SetDefaultModel(t.Context(), request)
	if err != nil || again != receipt || !reflect.DeepEqual(terminalBefore, terminalSession.rt.manager.View()) || bad.Calls() != 0 || rebuiltA.Calls() != 0 || rebuiltB.Calls() != 1 || runs.Load() != 1 {
		t.Fatalf("terminal default replay changed receipt or state: %+v %v", again, err)
	}
	assertReboundSelection(t, terminalSession, selected.ID, latest.ID, nil)
}
