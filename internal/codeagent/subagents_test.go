package codeagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	"github.com/ww1489/seasprak/internal/config"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func delegateSession(t *testing.T, sid string, main *testkit.FakeModel, child *captureModel, limits config.Limits) *AgentSession {
	t.Helper()
	root := agentRoots(t)
	opts := Options{Workspace: root + "/ws", StateRoot: root + "/state", SessionID: sid, Profile: ProfileMemory, Model: main, Principal: "local", GenerationFingerprint: "delegate-v1", Limits: limits,
		Agents: []agent.AgentDefinition{{Name: "reviewer", Version: "r1", Description: "reviews", Instruction: "You review.", Model: child, Delegable: true}}}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func delegateCall(agentName, task string) testkit.Step {
	args, _ := json.Marshal(map[string]string{"agent": agentName, "task": task})
	return testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "d1", Name: "delegate_task", Arguments: string(args)}}}
}

func delegateRecord(t *testing.T, s *AgentSession) agent.ToolRecord {
	t.Helper()
	for _, call := range s.rt.manager.View().Calls {
		if call.Call.Name == "delegate_task" {
			return call
		}
	}
	t.Fatal("delegate_task call was not recorded")
	return agent.ToolRecord{}
}

func submitPrompt(t *testing.T, s *AgentSession, text string) agent.InputReceipt {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"text": text})
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: raw})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestDelegateTaskRunsChildWithOnlyTaskText(t *testing.T) {
	main := testkit.NewFake(delegateCall("reviewer", "check the diff"), testkit.Step{Text: "parent done"})
	child := &captureModel{FakeModel: testkit.NewFake(testkit.Step{Text: "child says ok"})}
	s := delegateSession(t, "delegate-ok", main, child, config.Limits{})
	in := submitPrompt(t, s, "please delegate")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	if tr := view.Traces[in.TraceID]; tr.State != "completed" {
		t.Fatalf("trace %+v", tr)
	}
	if child.Calls() != 1 || main.Calls() != 2 {
		t.Fatalf("child=%d main=%d", child.Calls(), main.Calls())
	}
	// The child's own instruction is the system block; the only user input is the task.
	if len(child.seen) != 1 || strings.Join(child.seen[0], "|") != "You review.|check the diff" {
		t.Fatalf("child input %q", child.seen)
	}
	call := delegateRecord(t, s)
	if call.Observation == nil || call.Observation.Status != "succeeded" || call.Observation.SideEffect != "none" || !strings.Contains(call.Observation.Content, "child says ok") {
		t.Fatalf("parent tool result %+v", call.Observation)
	}
	for _, msg := range view.Messages {
		if msg.Kind == agent.KindAssistant && msg.Standard != nil {
			for _, block := range msg.Standard.ContentBlocks {
				if block != nil && block.AssistantGenText != nil && strings.Contains(block.AssistantGenText.Text, "child says ok") {
					t.Fatal("child assistant message entered parent history")
				}
			}
		}
		if msg.Standard != nil {
			for _, block := range msg.Standard.ContentBlocks {
				if block != nil && block.UserInputText != nil && block.UserInputText.Text == "check the diff" {
					t.Fatal("child task input entered parent history")
				}
			}
		}
	}
	if len(view.Invocations) != 1 {
		t.Fatalf("invocations %+v", view.Invocations)
	}
	for _, inv := range view.Invocations {
		if inv.State != "completed" || inv.ParentCallID != call.Call.CallID || inv.ParentInvocationID != view.Traces[in.TraceID].InvocationID || inv.TraceID != in.TraceID || inv.Target.Name != "reviewer" || inv.Target.Version != "r1" || inv.ModelCalls != 1 || inv.Result != "child says ok" || inv.ID == inv.ParentInvocationID {
			t.Fatalf("invocation %+v", inv)
		}
	}
	// Parent and child logical calls share the trace total without adding a parent Turn.
	if usage := view.Traces[in.TraceID].Usage; usage.LogicalModelCalls != 3 {
		t.Fatalf("shared usage %+v", usage)
	}
	turns := 0
	for _, turn := range view.Turns {
		if turn.TraceID == in.TraceID {
			turns++
		}
	}
	if turns != 2 {
		t.Fatalf("parent turns = %d", turns)
	}
}

func TestDelegateTaskSharesExhaustedTraceBudget(t *testing.T) {
	main := testkit.NewFake(delegateCall("reviewer", "check"), testkit.Step{Text: "never"})
	child := &captureModel{FakeModel: testkit.NewFake(testkit.Step{Text: "never"})}
	s := delegateSession(t, "delegate-budget", main, child, config.Limits{TraceLogicalModelCalls: 1})
	in := submitPrompt(t, s, "delegate")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	if child.Calls() != 0 || main.Calls() != 1 {
		t.Fatalf("child=%d main=%d", child.Calls(), main.Calls())
	}
	call := delegateRecord(t, s)
	if call.Observation == nil || call.Observation.SideEffect != "none" || !strings.Contains(call.Observation.Content, `"status":"failed"`) || !strings.Contains(call.Observation.Content, "budget_exhausted") {
		t.Fatalf("parent tool result %+v", call.Observation)
	}
	for _, inv := range view.Invocations {
		if inv.State != "failed" || inv.ModelCalls != 0 {
			t.Fatalf("invocation %+v", inv)
		}
	}
	if len(view.Invocations) != 1 {
		t.Fatalf("invocations %+v", view.Invocations)
	}
	tr := view.Traces[in.TraceID]
	if tr.State != "failed" || tr.Usage.LogicalModelCalls != 1 || view.HasUnresolvedEffects() {
		t.Fatalf("trace %+v unresolved=%v", tr, view.HasUnresolvedEffects())
	}
}

func TestDelegateTaskParentCancelCancelsChild(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	main := testkit.NewFake(delegateCall("reviewer", "slow"), testkit.Step{Text: "never"})
	child := &captureModel{FakeModel: testkit.NewFake(testkit.Step{Gate: gate, Text: "late"})}
	s := delegateSession(t, "delegate-cancel", main, child, config.Limits{})
	in := submitPrompt(t, s, "delegate")
	waitFor(t, func() bool { return child.Calls() == 1 })
	if err := s.Cancel(t.Context(), in.TraceID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	if tr := view.Traces[in.TraceID]; tr.State != "cancelled" {
		t.Fatalf("trace %+v", tr)
	}
	if len(view.Invocations) != 1 {
		t.Fatalf("invocations %+v", view.Invocations)
	}
	for _, inv := range view.Invocations {
		if inv.State != "cancelled" || inv.ModelCalls != 1 {
			t.Fatalf("invocation %+v", inv)
		}
	}
	if child.Calls() != 1 || main.Calls() != 1 || view.HasUnresolvedEffects() {
		t.Fatalf("child=%d main=%d unresolved=%v", child.Calls(), main.Calls(), view.HasUnresolvedEffects())
	}
}

func TestDelegateTaskUnknownAgentNeverRunsChild(t *testing.T) {
	main := testkit.NewFake(delegateCall("ghost", "x"), testkit.Step{Text: "done"})
	child := &captureModel{FakeModel: testkit.NewFake()}
	s := delegateSession(t, "delegate-unknown", main, child, config.Limits{})
	in := submitPrompt(t, s, "delegate")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	call := delegateRecord(t, s)
	if call.Observation == nil || call.Observation.Status == "succeeded" || call.Observation.Executed || call.Observation.SideEffect != "none" {
		t.Fatalf("unknown agent observation %+v", call.Observation)
	}
	if child.Calls() != 0 || len(s.rt.manager.View().Invocations) != 0 {
		t.Fatalf("child=%d invocations=%d", child.Calls(), len(s.rt.manager.View().Invocations))
	}
}

func TestDelegateTaskRejectedWhenCallerHasNoDelegates(t *testing.T) {
	// The only delegable target cannot delegate to itself; its segment hides the tool
	// and a forged call is rejected before any child runs.
	reviewer := testkit.NewFake(delegateCall("reviewer", "loop"), testkit.Step{Text: "done"})
	child := &captureModel{FakeModel: testkit.NewFake()}
	root := agentRoots(t)
	s, err := CreateAgentSession(t.Context(), Options{Workspace: root + "/ws", StateRoot: root + "/state", SessionID: "delegate-self", Profile: ProfileMemory, Model: child, Principal: "local", GenerationFingerprint: "delegate-v1",
		Agents: []agent.AgentDefinition{{Name: "reviewer", Version: "r1", Instruction: "You review.", Model: reviewer, Delegable: true}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", TargetAgent: "reviewer", Content: json.RawMessage(`{"text":"go"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	call := delegateRecord(t, s)
	if call.Observation == nil || call.Observation.Status == "succeeded" || call.Observation.Executed {
		t.Fatalf("self delegation observation %+v", call.Observation)
	}
	// The reviewer's inventory is empty, so the rejected call ends its run.
	if child.Calls() != 0 || reviewer.Calls() != 1 || len(s.rt.manager.View().Invocations) != 0 {
		t.Fatalf("child=%d reviewer=%d invocations=%d", child.Calls(), reviewer.Calls(), len(s.rt.manager.View().Invocations))
	}
}

func TestDelegateToolAbsentWithoutDelegableAgents(t *testing.T) {
	s := registrySession(t, agentRoots(t), "no-delegate", testkit.NewFake(), testkit.NewFake(), "r1", true)
	defer s.Close(t.Context())
	for _, info := range s.rt.opts.ToolInfos {
		if info.Name == "delegate_task" {
			t.Fatal("delegate_task exposed without delegable agents")
		}
	}
	for _, def := range s.rt.opts.Tools {
		if def.Name == "delegate_task" {
			t.Fatal("delegate_task registered without delegable agents")
		}
	}
	plain := Options{}
	plainDecls, err := alignTools(&plain)
	if err != nil {
		t.Fatal(err)
	}
	withAgents := Options{Agents: []agent.AgentDefinition{{Name: "reviewer", Version: "r1"}}}
	agentDecls, err := alignTools(&withAgents)
	if err != nil {
		t.Fatal(err)
	}
	a, err := buildManifest("", plainDecls, "fp")
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildManifest("", agentDecls, "fp")
	if err != nil || a.Hash != b.Hash {
		t.Fatalf("manifest changed without delegable agents: %v", err)
	}
	delegable := Options{Agents: []agent.AgentDefinition{{Name: "reviewer", Version: "r1", Delegable: true}}}
	decls, err := alignTools(&delegable)
	if err != nil || len(decls) != 1 || decls[0].Name != "delegate_task" || len(delegable.ToolInfos) != 1 {
		t.Fatalf("delegable manifest %+v %v", decls, err)
	}
	// Aligning an already aligned inventory stays idempotent (resume validation re-aligns).
	again, err := alignTools(&delegable)
	if err != nil || len(again) != 1 {
		t.Fatalf("realign %+v %v", again, err)
	}
}

func TestRunningInvocationInterruptedOnOpen(t *testing.T) {
	backend, err := memory.Open("inv-reopen", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	inv := state.Invocation{ID: "child", ParentInvocationID: "root", ParentCallID: "call", TraceID: "trace", Target: agent.TargetAgent{Name: "reviewer", Version: "r1"}, State: "running"}
	raw, _ := json.Marshal(inv)
	if _, err := backend.Append(t.Context(), "inv-reopen", store.ExpectedCommit{}, store.Commit{RecordType: "commit", Version: 1, CommitID: "c1", CommitSeq: 1,
		ControlRecords: []store.Record{{Type: "invocation", Version: 1, ID: "child", Payload: raw}}}); err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "inv-reopen")
	if err != nil {
		t.Fatal(err)
	}
	if manager.View().Invocations["child"].State != "running" {
		t.Fatalf("replayed %+v", manager.View().Invocations)
	}
	s, err := Start(Options{SessionID: "inv-reopen", Profile: ProfileMemory, Store: backend, Model: testkit.NewFake(), Principal: "local"}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	// A running child from an earlier process is interrupted (non-terminal),
	// keeping its parent identity; it is never continued or re-run.
	got := manager.View().Invocations["child"]
	if got.State != "interrupted" || got.ParentCallID != "call" || got.ParentInvocationID != "root" {
		t.Fatalf("recovered %+v", got)
	}
	got.State = "cancelled"
	if err := manager.SaveInvocation(t.Context(), got); err != nil {
		t.Fatalf("interrupted invocation cannot settle: %v", err)
	}
	// A terminal invocation is immutable.
	got.State = "completed"
	if err := manager.SaveInvocation(t.Context(), got); err == nil {
		t.Fatal("terminal invocation changed")
	}
}

func TestDelegationAdmissionLimits(t *testing.T) {
	rt := &runtime{}
	for i := 0; i < config.SubagentConcurrency; i++ {
		if err := rt.admitDelegation("t", 0); err != nil {
			t.Fatalf("admission %d: %v", i, err)
		}
	}
	if err := rt.admitDelegation("t", 0); err == nil {
		t.Fatal("concurrency limit not enforced")
	}
	if err := rt.admitDelegation("other", config.SubagentDepth); err == nil {
		t.Fatal("nesting limit not enforced")
	}
	rt.releaseDelegation("t")
	if err := rt.admitDelegation("t", config.SubagentDepth-1); err != nil {
		t.Fatalf("released slot: %v", err)
	}
}
