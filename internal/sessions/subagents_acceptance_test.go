package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/testkit"
)

func subagentOptions(root, sid string, main *testkit.FakeModel, agents []agent.AgentDefinition, defs []tools.Definition) Options {
	return Options{Workspace: root + "/ws", StateRoot: root + "/state", SessionID: sid, Profile: ProfileMemory, Model: main, Principal: "local", GenerationFingerprint: "subagent-v1", Agents: agents, Tools: defs}
}

func openSubagentSession(t *testing.T, opts Options, create bool) *AgentSession {
	t.Helper()
	open := OpenAgentSession
	if create {
		open = CreateAgentSession
	}
	s, err := open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// countedTool is a trusted-run tool; a non-nil fail makes its effect unknown.
func countedTool(name string, count *atomic.Int32, fail error) tools.Definition {
	return tools.Definition{Name: name, Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) {
		count.Add(1)
		return name + "-result", fail
	}}
}

func toolCalls(calls ...schema.FunctionToolCall) testkit.Step {
	return testkit.Step{ToolCalls: calls}
}

func delegateArgs(agentName, task string) string {
	raw, _ := json.Marshal(map[string]string{"agent": agentName, "task": task})
	return string(raw)
}

func callsNamed(v state.View, name string) []agent.ToolRecord {
	var out []agent.ToolRecord
	for _, call := range v.Calls {
		if call.Call.Name == name {
			out = append(out, call)
		}
	}
	return out
}

func delegateOutcome(t *testing.T, call agent.ToolRecord) delegateResult {
	t.Helper()
	var out delegateResult
	if call.Observation == nil || json.Unmarshal([]byte(call.Observation.Content), &out) != nil {
		t.Fatalf("delegate observation %+v", call.Observation)
	}
	return out
}

func onlyInvocation(t *testing.T, v state.View) state.Invocation {
	t.Helper()
	if len(v.Invocations) != 1 {
		t.Fatalf("invocations %+v", v.Invocations)
	}
	for _, inv := range v.Invocations {
		return inv
	}
	return state.Invocation{}
}

func TestDelegatedChildRunsOnlyDeclaredTools(t *testing.T) {
	var probe, secret atomic.Int32
	main := testkit.NewFake(delegateCall("worker", "inspect the tree"), testkit.Step{Text: "parent done"})
	child := testkit.NewFake(
		testkit.Step{Text: "child thinking aloud", ToolCalls: []schema.FunctionToolCall{{CallID: "p1", Name: "probe", Arguments: `{}`}, {CallID: "s1", Name: "secret", Arguments: `{}`}}},
		testkit.Step{Text: "child final"})
	agents := []agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "You work.", Model: child, Delegable: true, Tools: []string{"probe"}}}
	s := openSubagentSession(t, subagentOptions(agentRoots(t), "child-tools", main, agents, []tools.Definition{countedTool("probe", &probe, nil), countedTool("secret", &secret, nil)}), true)
	in := submitPrompt(t, s, "delegate")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	tr := view.Traces[in.TraceID]
	if tr.State != "completed" || main.Calls() != 2 || child.Calls() != 2 {
		t.Fatalf("trace=%s main=%d child=%d", tr.State, main.Calls(), child.Calls())
	}
	if probe.Load() != 1 || secret.Load() != 0 {
		t.Fatalf("probe=%d secret=%d", probe.Load(), secret.Load())
	}
	inv := onlyInvocation(t, view)
	if inv.State != "completed" || inv.ParentInvocationID != tr.InvocationID || inv.ModelCalls != 2 || inv.Result != "child final" {
		t.Fatalf("invocation %+v", inv)
	}
	probes := callsNamed(view, "probe")
	if len(probes) != 1 {
		t.Fatalf("probe records %+v", probes)
	}
	p := probes[0]
	if p.Scope.InvocationID != inv.ID || p.Scope.ParentInvocationID != tr.InvocationID || p.Scope.TurnID != "" || p.Scope.SelectionRevision != 0 || p.Call.ProviderCallID != "p1" || !p.Claimed || p.Observation == nil || p.Observation.Status != "succeeded" {
		t.Fatalf("child probe record %+v obs=%+v", p, p.Observation)
	}
	// The undeclared tool is rejected before it starts, under the child scope.
	for _, c := range callsNamed(view, "secret") {
		if c.Scope.InvocationID != inv.ID || c.Claimed || c.Observation == nil || c.Observation.Executed || c.Observation.Status == "succeeded" || c.Observation.SideEffect != "none" {
			t.Fatalf("undeclared child tool record %+v obs=%+v", c, c.Observation)
		}
	}
	if tr.Usage.ToolExecutions != 2 || view.HasUnresolvedEffects() {
		t.Fatalf("usage %+v unresolved=%v", tr.Usage, view.HasUnresolvedEffects())
	}
	// Only the delegate result reaches the parent; no child message does.
	snap, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(snap.Messages)
	for _, leaked := range []string{"child thinking aloud", "probe-result", `"p1"`, `"s1"`} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("child content %q entered parent history", leaked)
		}
	}
	for _, msg := range snap.Messages {
		if msg.Scope.InvocationID == inv.ID {
			t.Fatalf("child-scoped message in parent history %+v", msg)
		}
	}
}

func TestNestedDelegationChainsInvocations(t *testing.T) {
	main := testkit.NewFake(delegateCall("alpha", "plan"), testkit.Step{Text: "parent done"})
	alpha := testkit.NewFake(delegateCall("beta", "detail"), testkit.Step{Text: "alpha done"})
	beta := testkit.NewFake(testkit.Step{Text: "beta done"})
	agents := []agent.AgentDefinition{
		{Name: "alpha", Version: "a1", Instruction: "A", Model: alpha, Delegable: true, Tools: []string{delegateToolName}},
		{Name: "beta", Version: "b1", Instruction: "B", Model: beta, Delegable: true},
	}
	s := openSubagentSession(t, subagentOptions(agentRoots(t), "nested", main, agents, nil), true)
	in := submitPrompt(t, s, "delegate")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	tr := view.Traces[in.TraceID]
	if tr.State != "completed" || main.Calls() != 2 || alpha.Calls() != 2 || beta.Calls() != 1 {
		t.Fatalf("trace=%s main=%d alpha=%d beta=%d", tr.State, main.Calls(), alpha.Calls(), beta.Calls())
	}
	if len(view.Invocations) != 2 {
		t.Fatalf("invocations %+v", view.Invocations)
	}
	var a, b state.Invocation
	for _, inv := range view.Invocations {
		switch inv.Target.Name {
		case "alpha":
			a = inv
		case "beta":
			b = inv
		}
	}
	if a.State != "completed" || a.ParentInvocationID != tr.InvocationID || a.ModelCalls != 2 || a.Result != "alpha done" {
		t.Fatalf("alpha invocation %+v", a)
	}
	if b.State != "completed" || b.ParentInvocationID != a.ID || b.TraceID != in.TraceID || b.ModelCalls != 1 || b.Result != "beta done" {
		t.Fatalf("beta invocation %+v", b)
	}
	// Beta's parent call is alpha's own registered delegate_task call.
	parentCall := view.Calls[b.ParentCallID]
	if parentCall.Call.Name != delegateToolName || parentCall.Scope.InvocationID != a.ID || parentCall.Scope.TurnID != "" || !strings.Contains(parentCall.Observation.Content, "beta done") {
		t.Fatalf("beta parent call %+v", parentCall)
	}
	if tr.Usage.LogicalModelCalls != 5 {
		t.Fatalf("shared usage %+v", tr.Usage)
	}
}

func TestParallelDelegationsBeyondConcurrencyAreDenied(t *testing.T) {
	gate := make(chan struct{})
	var calls []schema.FunctionToolCall
	for i := 0; i <= config.SubagentConcurrency; i++ {
		calls = append(calls, schema.FunctionToolCall{CallID: "d" + string(rune('1'+i)), Name: delegateToolName, Arguments: delegateArgs("reviewer", "task")})
	}
	main := testkit.NewFake(toolCalls(calls...), testkit.Step{Text: "parent done"})
	child := testkit.NewFake(testkit.Step{Gate: gate, Text: "child ok", Repeat: true})
	agents := []agent.AgentDefinition{{Name: "reviewer", Version: "r1", Instruction: "R", Model: child, Delegable: true}}
	s := openSubagentSession(t, subagentOptions(agentRoots(t), "parallel", main, agents, nil), true)
	in := submitPrompt(t, s, "fan out")
	// All admitted children hold their slot while the extra call is denied.
	waitFor(t, func() bool {
		if child.Calls() != config.SubagentConcurrency {
			return false
		}
		for _, call := range callsNamed(s.rt.manager.View(), delegateToolName) {
			if call.Observation != nil {
				return true
			}
		}
		return false
	})
	close(gate)
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	if tr := view.Traces[in.TraceID]; tr.State != "completed" || main.Calls() != 2 {
		t.Fatalf("trace=%s main=%d", tr.State, main.Calls())
	}
	if child.Calls() != config.SubagentConcurrency || len(view.Invocations) != config.SubagentConcurrency {
		t.Fatalf("child=%d invocations=%d", child.Calls(), len(view.Invocations))
	}
	completed, denied := 0, 0
	for _, call := range callsNamed(view, delegateToolName) {
		out := delegateOutcome(t, call)
		switch {
		case out.Status == "completed" && out.Result == "child ok" && view.Invocations[out.InvocationID].State == "completed":
			completed++
		case out.Status == "denied" && out.Code == product.CodeBudgetExhausted && out.InvocationID == "":
			denied++
		default:
			t.Fatalf("delegate result %+v", out)
		}
	}
	if completed != config.SubagentConcurrency || denied != 1 {
		t.Fatalf("completed=%d denied=%d", completed, denied)
	}
}

// A linear chain at the depth limit also holds every per-trace concurrency
// slot (SubagentDepth == SubagentConcurrency), so either limit denies it.
func TestDelegationBeyondDepthIsDenied(t *testing.T) {
	names := []string{"a1", "a2", "a3", "a4", "a5"}
	models := map[string]*testkit.FakeModel{}
	var agents []agent.AgentDefinition
	for i, name := range names {
		def := agent.AgentDefinition{Name: name, Version: "v1", Instruction: name, Delegable: true}
		if i+1 < len(names) {
			models[name] = testkit.NewFake(delegateCall(names[i+1], "go deeper"), testkit.Step{Text: name + " done"})
			def.Tools = []string{delegateToolName}
		} else {
			models[name] = testkit.NewFake(testkit.Step{Text: "too deep"})
		}
		def.Model = models[name]
		agents = append(agents, def)
	}
	main := testkit.NewFake(delegateCall("a1", "start"), testkit.Step{Text: "parent done"})
	s := openSubagentSession(t, subagentOptions(agentRoots(t), "depth", main, agents, nil), true)
	in := submitPrompt(t, s, "delegate")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	tr := view.Traces[in.TraceID]
	if tr.State != "completed" || main.Calls() != 2 || models["a5"].Calls() != 0 {
		t.Fatalf("trace=%s main=%d a5=%d", tr.State, main.Calls(), models["a5"].Calls())
	}
	if len(view.Invocations) != config.SubagentDepth {
		t.Fatalf("invocations %d", len(view.Invocations))
	}
	for _, name := range names[:config.SubagentDepth] {
		if models[name].Calls() != 2 {
			t.Fatalf("%s calls=%d", name, models[name].Calls())
		}
	}
	var deepest state.Invocation
	for _, inv := range view.Invocations {
		if inv.State != "completed" {
			t.Fatalf("invocation %+v", inv)
		}
		if inv.Target.Name == "a4" {
			deepest = inv
		}
	}
	var denied []delegateResult
	for _, call := range callsNamed(view, delegateToolName) {
		if call.Scope.InvocationID == deepest.ID {
			denied = append(denied, delegateOutcome(t, call))
		}
	}
	if len(denied) != 1 || denied[0].Status != "denied" || denied[0].Code != product.CodeBudgetExhausted {
		t.Fatalf("deepest delegation %+v", denied)
	}
}

func TestChildUnknownEffectRequiresParentReconciliation(t *testing.T) {
	var effect atomic.Int32
	main := testkit.NewFake(delegateCall("worker", "write it"), testkit.Step{Text: "never"})
	child := testkit.NewFake(toolCalls(schema.FunctionToolCall{CallID: "w1", Name: "effect", Arguments: `{}`}), testkit.Step{Text: "child final"})
	agents := []agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"effect"}}}
	s := openSubagentSession(t, subagentOptions(agentRoots(t), "child-unknown", main, agents, []tools.Definition{countedTool("effect", &effect, errors.New("lost connection"))}), true)
	in := submitPrompt(t, s, "delegate")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	if effect.Load() != 1 || child.Calls() != 2 || main.Calls() != 1 {
		t.Fatalf("effect=%d child=%d main=%d", effect.Load(), child.Calls(), main.Calls())
	}
	inv := onlyInvocation(t, view)
	child1 := callsNamed(view, "effect")
	if len(child1) != 1 || child1[0].Scope.InvocationID != inv.ID || child1[0].Observation == nil || child1[0].Observation.SideEffect != "unknown" {
		t.Fatalf("child effect record %+v", child1)
	}
	parent := delegateRecord(t, s)
	if parent.Observation == nil || parent.Observation.SideEffect != "unknown" || parent.Observation.Status == "succeeded" {
		t.Fatalf("parent delegate observation %+v", parent.Observation)
	}
	if !view.HasUnresolvedEffects() || !view.TraceHasUnresolvedEffects(in.TraceID) {
		t.Fatal("parent trace does not require reconciliation")
	}
	before := view.LastSeq
	_, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"next"}`)})
	requireCode(t, err, product.CodeReconciliationRequired)
	if s.rt.manager.View().LastSeq != before || main.Calls() != 1 {
		t.Fatal("rejected input wrote state or ran")
	}
	evidence, err := s.rt.delegateEvidence(t.Context(), ReconcileQueryRequest{TraceID: in.TraceID, CallID: parent.Call.CallID})
	if err != nil || evidence.ConfirmedExecution || evidence.TrustedNoStart || len(evidence.RemainingUnknown) != 1 || !strings.Contains(evidence.RemainingUnknown[0], child1[0].Call.CallID) {
		t.Fatalf("evidence %+v %v", evidence, err)
	}
}

func TestParentCloseInterruptsRunningChild(t *testing.T) {
	root := agentRoots(t)
	gate := make(chan struct{})
	defer close(gate)
	main := testkit.NewFake(delegateCall("reviewer", "slow"), testkit.Step{Text: "never"})
	child := testkit.NewFake(testkit.Step{Gate: gate, Text: "late"})
	agents := []agent.AgentDefinition{{Name: "reviewer", Version: "r1", Instruction: "R", Model: child, Delegable: true}}
	opts := subagentOptions(root, "close-running", main, agents, nil)
	s := openSubagentSession(t, opts, true)
	in := submitPrompt(t, s, "delegate")
	waitFor(t, func() bool { return child.Calls() == 1 })
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopenedMain, reopenedChild := testkit.NewFake(), testkit.NewFake()
	opts.Model, opts.Agents[0].Model = reopenedMain, reopenedChild
	s = openSubagentSession(t, opts, false)
	view := s.rt.manager.View()
	inv := onlyInvocation(t, view)
	if inv.State != "interrupted" || inv.TraceID != in.TraceID {
		t.Fatalf("invocation %+v", inv)
	}
	if tr := view.Traces[in.TraceID]; tr.State != "paused" {
		t.Fatalf("trace %+v", tr)
	}
	parent := delegateRecord(t, s)
	if inv.ParentCallID != parent.Call.CallID {
		t.Fatalf("invocation parent call %s != %s", inv.ParentCallID, parent.Call.CallID)
	}
	evidence, err := s.rt.delegateEvidence(t.Context(), ReconcileQueryRequest{TraceID: in.TraceID, CallID: parent.Call.CallID})
	if err != nil || !evidence.TrustedNoStart || evidence.ConfirmedExecution || len(evidence.EvidenceRefs) != 1 || evidence.EvidenceRefs[0] != "invocation:"+inv.ID {
		t.Fatalf("evidence %+v %v", evidence, err)
	}
	if child.Calls() != 1 || reopenedChild.Calls() != 0 || reopenedMain.Calls() != 0 || main.Calls() != 1 {
		t.Fatalf("child=%d reopenedChild=%d reopenedMain=%d main=%d", child.Calls(), reopenedChild.Calls(), reopenedMain.Calls(), main.Calls())
	}
}

func TestCompletedChildIsNotRerunAfterReopen(t *testing.T) {
	root := agentRoots(t)
	gate := make(chan struct{})
	defer close(gate)
	main := testkit.NewFake(delegateCall("reviewer", "check"), testkit.Step{Gate: gate, Text: "never"})
	child := testkit.NewFake(testkit.Step{Text: "child ok"})
	agents := []agent.AgentDefinition{{Name: "reviewer", Version: "r1", Instruction: "R", Model: child, Delegable: true}}
	opts := subagentOptions(root, "close-completed", main, agents, nil)
	s := openSubagentSession(t, opts, true)
	in := submitPrompt(t, s, "delegate")
	// Close while the parent's next model call runs after the child completed.
	waitFor(t, func() bool { return main.Calls() == 2 })
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopenedMain, reopenedChild := testkit.NewFake(testkit.Step{Text: "parent done"}), testkit.NewFake()
	opts.Model, opts.Agents[0].Model = reopenedMain, reopenedChild
	s = openSubagentSession(t, opts, false)
	view := s.rt.manager.View()
	inv := onlyInvocation(t, view)
	if inv.State != "completed" || inv.Result != "child ok" || inv.ModelCalls != 1 {
		t.Fatalf("invocation %+v", inv)
	}
	parent := delegateRecord(t, s)
	if out := delegateOutcome(t, parent); out.Status != "completed" || out.InvocationID != inv.ID || out.Result != "child ok" {
		t.Fatalf("parent result %+v", out)
	}
	evidence, err := s.rt.delegateEvidence(t.Context(), ReconcileQueryRequest{TraceID: in.TraceID, CallID: parent.Call.CallID})
	if err != nil || !evidence.ConfirmedExecution || evidence.TrustedNoStart || len(evidence.RemainingUnknown) != 0 || len(evidence.EvidenceRefs) != 1 || evidence.EvidenceRefs[0] != "invocation:"+inv.ID || evidence.EvidenceSource != "delegation-invocation" {
		t.Fatalf("evidence %+v %v", evidence, err)
	}
	// A Close during a parent model call leaves no pause checkpoint, so the
	// trace cannot resume; nothing is re-run or written.
	_, err = s.Resume(t.Context(), ResumeCommand{TraceID: in.TraceID, ExpectedRevision: view.LastSeq})
	requireCode(t, err, product.CodeIncompatibleResume)
	after := s.rt.manager.View()
	if after.LastSeq != view.LastSeq || after.Invocations[inv.ID].State != "completed" {
		t.Fatalf("rejected resume changed state: seq %d->%d inv=%+v", view.LastSeq, after.LastSeq, after.Invocations[inv.ID])
	}
	if child.Calls() != 1 || reopenedChild.Calls() != 0 || reopenedMain.Calls() != 0 {
		t.Fatalf("child=%d reopenedChild=%d reopenedMain=%d", child.Calls(), reopenedChild.Calls(), reopenedMain.Calls())
	}
}

// An interrupted child with no claimed child tool is resumed from its durable
// invocation record, not from a manufactured Eino checkpoint. The original
// invocation and parent delegate_task call are reused; after the child result
// is committed, the parent continues from that tool result.
func TestInterruptedChildResumesOriginalInvocationAfterStoppedParent(t *testing.T) {
	root := agentRoots(t)
	gate := make(chan struct{})
	defer close(gate)
	main := testkit.NewFake(delegateCall("reviewer", "slow"), testkit.Step{Text: "never"})
	child := testkit.NewFake(testkit.Step{Gate: gate, Text: "late"})
	agents := []agent.AgentDefinition{{Name: "reviewer", Version: "r1", Instruction: "R", Model: child, Delegable: true}}
	opts := subagentOptions(root, "interrupted-resume", main, agents, nil)
	opts.Model = versionedPauseModel{main}
	opts.Agents[0].Model = versionedPauseModel{child}
	s := openSubagentSession(t, opts, true)
	in := submitPrompt(t, s, "delegate")
	waitFor(t, func() bool { return child.Calls() == 1 })
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	reopenedMain := testkit.NewFake(testkit.Step{Text: "parent done"})
	reopenedChild := testkit.NewFake(testkit.Step{Text: "child resumed"})
	opts.Model, opts.Agents[0].Model = versionedPauseModel{reopenedMain}, versionedPauseModel{reopenedChild}
	s = openSubagentSession(t, opts, false)
	before := s.rt.manager.View()
	inv := onlyInvocation(t, before)
	tr := before.Traces[in.TraceID]
	parent := delegateRecord(t, s)
	if inv.State != "interrupted" || tr.State != "paused" || !tr.ExecutionStopped {
		t.Fatalf("unsafe stopped state: invocation=%+v trace=%+v", inv, tr)
	}
	if tr.CheckpointID != "" || len(before.Checkpoints) != 0 {
		t.Fatalf("child recovery manufactured a checkpoint: trace=%+v checkpoints=%+v", tr, before.Checkpoints)
	}
	if parent.Observation != nil {
		t.Fatalf("safe interrupted delegation was prematurely finalized: %+v", parent.Observation)
	}
	snap, err := s.Snapshot(t.Context())
	if err != nil || !snap.Resume[in.TraceID].CanResume {
		t.Fatalf("safe child recovery is not eligible: %+v err=%v", snap.Resume[in.TraceID], err)
	}
	receipt, err := s.Resume(t.Context(), ResumeCommand{TraceID: in.TraceID, ExpectedRevision: before.LastSeq, IdempotencyKey: "resume-child"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	after := s.rt.manager.View()
	if got := after.Invocations[inv.ID]; got.State != "completed" || got.Result != "child resumed" || got.ModelCalls != inv.ModelCalls+1 {
		t.Fatalf("resumed invocation %+v before=%+v", got, inv)
	}
	if call := after.Calls[parent.Call.CallID]; call.Call != parent.Call || call.Scope != parent.Scope || call.Observation == nil || call.Observation.Status != "succeeded" || call.Observation.SideEffect != "none" || !strings.Contains(call.Observation.Content, "child resumed") {
		t.Fatalf("original parent call was not completed: before=%+v after=%+v", parent, call)
	}
	if after.Traces[in.TraceID].State != "completed" || child.Calls() != 1 || reopenedChild.Calls() != 1 || reopenedMain.Calls() != 1 {
		t.Fatalf("trace=%+v child=%d reopenedChild=%d reopenedMain=%d", after.Traces[in.TraceID], child.Calls(), reopenedChild.Calls(), reopenedMain.Calls())
	}
	status, err := s.GetOperation(t.Context(), receipt.OperationID)
	if err != nil || status.State != "completed" {
		t.Fatalf("resume operation=%+v err=%v", status, err)
	}
	if len(after.Invocations) != 1 || len(callsNamed(after, delegateToolName)) != 1 || after.Inputs[in.InputID].State != "consumed" {
		t.Fatalf("identity duplicated: invocations=%d calls=%d input=%+v", len(after.Invocations), len(callsNamed(after, delegateToolName)), after.Inputs[in.InputID])
	}
	toolResults := 0
	for _, msg := range after.Messages {
		if msg.Kind == agent.KindToolResult && msg.Scope.ToolCallID == parent.Call.CallID {
			toolResults++
		}
	}
	if toolResults != 1 {
		t.Fatalf("parent tool results=%d", toolResults)
	}
}

// childWindowModel is a delegated child's model: it records every request's
// visible texts and declares a resolved context window, so the child's own
// soft threshold is evaluated exactly like the parent's.
type childWindowModel struct {
	*captureModel
	window int
}

func (m *childWindowModel) EffectiveOptions() llm.EffectiveOptions {
	return llm.EffectiveOptions{ContextWindowTokens: m.window, MaxOutputTokens: 16}
}

// childSummaryRequests counts the requests whose material is summary material.
func childSummaryRequests(c *captureModel) int {
	n := 0
	for _, texts := range c.seen {
		for _, text := range texts {
			if strings.Contains(text, "<history>") {
				n++
				break
			}
		}
	}
	return n
}

// TestChildCompactsItsOwnContextWithoutTouchingParent asserts the P3 rule that
// a delegated child compacts only its own invocation projection (08 §6): the
// summary is produced through the shared Eino compaction service, counted in
// the parent trace budget, and neither the summary nor any child message is
// appended to the parent branch.
func TestChildCompactsItsOwnContextWithoutTouchingParent(t *testing.T) {
	var probe atomic.Int32
	// The first two rounds stay far below the child's window; the third returns
	// a large result that crosses the soft threshold exactly once.
	big := tools.Definition{Name: "probe", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) {
		if probe.Add(1) < 3 {
			return "ok", nil
		}
		return strings.Repeat("o", 30000), nil
	}}
	main := testkit.NewFake(delegateCall("worker", "inspect the whole tree"), testkit.Step{Text: "parent done"})
	call := func(id string) testkit.Step {
		return testkit.Step{Text: "child step " + id, ToolCalls: []schema.FunctionToolCall{{CallID: id, Name: "probe", Arguments: `{}`}}}
	}
	// Requests 1-3 fit; the fourth boundary compacts, so the summary request is
	// the fourth model call and the replacement request is the fifth.
	child := &childWindowModel{captureModel: &captureModel{FakeModel: testkit.NewFake(
		call("c1"), call("c2"), call("c3"),
		testkit.Step{Text: summaryText()},
		testkit.Step{Text: "child final"})}, window: 8192}
	agents := []agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "You work.", Model: child, Delegable: true, Tools: []string{"probe"}}}
	s := openSubagentSession(t, subagentOptions(agentRoots(t), "child-compaction", main, agents, []tools.Definition{big}), true)
	in := submitPrompt(t, s, "delegate")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	tr := view.Traces[in.TraceID]
	if tr.State != "completed" || probe.Load() != 3 {
		t.Fatalf("trace=%s err=%q probe=%d", tr.State, tr.Error, probe.Load())
	}
	// Exactly one summary request: the child compacts once, not in a loop.
	if got := childSummaryRequests(child.captureModel); got != 1 {
		t.Fatalf("child summary requests=%d, want 1 (requests=%d)", got, len(child.seen))
	}
	if child.Calls() != 5 {
		t.Fatalf("child model calls=%d, want 5", child.Calls())
	}
	summary := strings.Join(child.seen[3], "|")
	replacement := strings.Join(child.seen[4], "|")
	if !strings.Contains(summary, "child step c1") || !strings.Contains(summary, "inspect the whole tree") {
		t.Fatalf("child summary material did not come from the child's own conversation: %s", summary)
	}
	// The replacement request carries the summary in place of the early rounds.
	if !strings.Contains(replacement, "## Goal") || strings.Contains(replacement, "child step c1") {
		t.Fatalf("child request did not use its compacted projection: %s", replacement)
	}
	inv := onlyInvocation(t, view)
	if inv.State != "completed" || inv.Result != "child final" {
		t.Fatalf("invocation %+v", inv)
	}
	// The summary is charged to the shared trace ledger: 2 parent calls plus 5
	// child calls (4 agent requests and the summary) with no extra parent Turn.
	if tr.Usage.LogicalModelCalls != 7 {
		t.Fatalf("shared usage did not include the child summary: %+v", tr.Usage)
	}
	turns := 0
	for _, turn := range view.Turns {
		if turn.TraceID == in.TraceID {
			turns++
		}
	}
	if turns != 2 {
		t.Fatalf("child compaction created parent turns: %d", turns)
	}
	// No child material and no child summary entered the parent branch.
	for _, msg := range view.Messages {
		if msg.Scope.InvocationID == inv.ID {
			t.Fatalf("child-scoped message in parent history %+v", msg)
		}
		if msg.Kind == agent.KindCompactionSummary {
			t.Fatalf("child compaction wrote a parent summary %+v", msg)
		}
	}
	// Only the delegate result ("child final") reaches the parent. The child's
	// intermediate steps, tool output and summary never do.
	raw, _ := json.Marshal(view.Messages)
	for _, leaked := range []string{"child step c1", "## Goal", `"c1"`, "ooooooooo"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("child compaction leaked %q into parent history", leaked)
		}
	}
	// A child compaction is not a parent maintenance operation either.
	for _, op := range view.Operations {
		if op.Kind == "compact" {
			t.Fatalf("child compaction created a parent compact operation %+v", op)
		}
	}
	if s.rt.manager.TraceCompactions(in.TraceID) != 0 {
		t.Fatal("child compaction counted against the parent trace limit")
	}
}

func TestPausedParentResumeReusesCompletedChild(t *testing.T) {
	root := agentRoots(t)
	gate := make(chan struct{})
	main := testkit.NewFake(delegateCall("reviewer", "check"), testkit.Step{Text: "never"})
	child := testkit.NewFake(testkit.Step{Gate: gate, Text: "child ok"})
	agents := []agent.AgentDefinition{{Name: "reviewer", Version: "r1", Instruction: "R", Model: child, Delegable: true}}
	opts := subagentOptions(root, "pause-completed", main, agents, nil)
	opts.Model = versionedPauseModel{main} // Resume requires a versioned model configuration
	s := openSubagentSession(t, opts, true)
	in := submitPrompt(t, s, "delegate")
	waitFor(t, func() bool { return child.Calls() == 1 })
	paused := make(chan error, 1)
	go func() {
		_, err := s.Pause(context.Background(), in.TraceID)
		paused <- err
	}()
	waitFor(t, func() bool {
		v, err := s.rt.call(t.Context(), func(rt *runtime) (any, error) { return rt.active != nil && rt.active.pauseID != "", nil })
		return err == nil && v.(bool)
	})
	close(gate) // the running child finishes; the parent stops before its next model call
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopenedMain, reopenedChild := testkit.NewFake(testkit.Step{Text: "parent done"}), testkit.NewFake()
	opts.Model, opts.Agents[0].Model = versionedPauseModel{reopenedMain}, reopenedChild
	s = openSubagentSession(t, opts, false)
	view := s.rt.manager.View()
	inv := onlyInvocation(t, view)
	if inv.State != "completed" || inv.Result != "child ok" || main.Calls() != 1 {
		t.Fatalf("invocation %+v main=%d", inv, main.Calls())
	}
	resumeWorkflowTrace(t, s, in.TraceID)
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view = s.rt.manager.View()
	if tr := view.Traces[in.TraceID]; tr.State != "completed" {
		t.Fatalf("resumed trace %+v", tr)
	}
	// The parent continues from the saved delegate result; the child never re-runs.
	if child.Calls() != 1 || reopenedChild.Calls() != 0 || reopenedMain.Calls() != 1 || len(view.Invocations) != 1 || len(callsNamed(view, delegateToolName)) != 1 {
		t.Fatalf("child=%d reopenedChild=%d reopenedMain=%d invocations=%d", child.Calls(), reopenedChild.Calls(), reopenedMain.Calls(), len(view.Invocations))
	}
}
