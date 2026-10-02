package codeagent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestApprovalUnknownResultReopenDoesNotReexecute(t *testing.T) {
	var runs atomic.Int32
	model := versionedPauseModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "work", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "done"})}
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Principal: "host", GenerationFingerprint: "approval-unknown-reopen", Model: model, Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		return "", errors.New("uncertain fixture result")
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
	waitResumeCondition(t, func() bool { return s.rt.manager.View().Traces[input.TraceID].State == "paused" })
	answerCommand(t, s, "allowed-once")
	if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: s.rt.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[input.TraceID].State) })
	before := s.rt.manager.View()
	if !before.HasUnresolvedEffects() || runs.Load() != 1 || before.Traces[input.TraceID].Usage.ToolExecutions != 1 || len(before.Calls) != 1 {
		t.Fatal("unknown result lost durable claim or budget")
	}
	for _, call := range before.Calls {
		if !call.Claimed || call.Observation == nil || !call.Observation.Executed || call.Observation.SideEffect != "unknown" {
			t.Fatal("unknown execution fact missing")
		}
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := model.Calls()
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	after := opened.rt.manager.View()
	if runs.Load() != 1 || model.Calls() != calls || !after.HasUnresolvedEffects() || !reflect.DeepEqual(before.Calls, after.Calls) || after.Traces[input.TraceID].Usage.ToolExecutions != 1 || len(approvalSnapshot(t, opened).Interactions) != 0 {
		t.Fatal("reopen forgot unknown or reasked an already claimed operation")
	}
	_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: after.LastSeq})
	requireSessionCode(t, err, product.CodeIncompatibleResume)
	if runs.Load() != 1 || model.Calls() != calls || !reflect.DeepEqual(after, opened.rt.manager.View()) {
		t.Fatal("unknown work executed twice")
	}
}
