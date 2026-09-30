package sessions

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/testkit"
)

func wfLiteral(v string) agent.WorkflowValue { return agent.WorkflowValue{Literal: json.RawMessage(v)} }
func wfOutput(node, field string) agent.WorkflowValue {
	return agent.WorkflowValue{Ref: &agent.WorkflowRef{Node: node, Field: field}}
}

const wfInputSchema = `{"type":"object","properties":{"name":{"type":"string"},"score":{"type":"number"}},"required":["name","score"]}`

// s -> lit -> t(echo) -> c(score > 50); true -> big(shout) -> e, false -> e.
func triageWorkflow() agent.WorkflowDefinition {
	return agent.WorkflowDefinition{Name: "triage", Version: "w1", Description: "triage input", Source: "test", FormatVersion: agent.WorkflowFormatV1,
		InputSchema: json.RawMessage(wfInputSchema),
		Nodes: []agent.WorkflowNode{
			{ID: "s", Type: agent.WorkflowNodeStart},
			{ID: "lit", Type: agent.WorkflowNodeLiteral, Inputs: map[string]agent.WorkflowValue{"greeting": wfLiteral(`"hi"`)}},
			{ID: "t", Type: agent.WorkflowNodeTool, Tool: "echo", Inputs: map[string]agent.WorkflowValue{"q": wfOutput("s", "name"), "g": wfOutput("lit", "greeting")}},
			{ID: "c", Type: agent.WorkflowNodeCondition, Condition: &agent.WorkflowCondition{Op: "gt", Left: wfOutput("s", "score"), Right: wfLiteral("50")}},
			{ID: "big", Type: agent.WorkflowNodeTool, Tool: "shout", Inputs: map[string]agent.WorkflowValue{"text": wfOutput("t", "result")}},
			{ID: "e", Type: agent.WorkflowNodeEnd, Inputs: map[string]agent.WorkflowValue{"name": wfOutput("s", "name"), "echo": wfOutput("t", "result")}},
		},
		Edges: []agent.WorkflowEdge{{From: "s", To: "lit"}, {From: "lit", To: "t"}, {From: "t", To: "c"}, {From: "c", To: "big", Port: "true"}, {From: "c", To: "e", Port: "false"}, {From: "big", To: "e"}},
	}
}

// s -> t(echo) -> m(model) -> e.
func summarizeWorkflow() agent.WorkflowDefinition {
	return agent.WorkflowDefinition{Name: "summarize", Version: "w1", Source: "test", FormatVersion: agent.WorkflowFormatV1,
		InputSchema: json.RawMessage(wfInputSchema),
		Nodes: []agent.WorkflowNode{
			{ID: "s", Type: agent.WorkflowNodeStart},
			{ID: "t", Type: agent.WorkflowNodeTool, Tool: "echo", Inputs: map[string]agent.WorkflowValue{"q": wfOutput("s", "name")}},
			{ID: "m", Type: agent.WorkflowNodeModel, Model: WorkflowModelBinding, Prompt: "summarize {{x}}", Inputs: map[string]agent.WorkflowValue{"x": wfOutput("t", "result")}},
			{ID: "e", Type: agent.WorkflowNodeEnd, Inputs: map[string]agent.WorkflowValue{"summary": wfOutput("m", "text")}},
		},
		Edges: []agent.WorkflowEdge{{From: "s", To: "t"}, {From: "t", To: "m"}, {From: "m", To: "e"}},
	}
}

type wfTools struct {
	echo, shout atomic.Int32
	gate        chan struct{} // optional: blocks echo until closed
}

func (w *wfTools) definitions(echoSchema string) []tools.Definition {
	if echoSchema == "" {
		echoSchema = `{"type":"object"}`
	}
	return []tools.Definition{
		{Name: "echo", Version: "1", Schema: json.RawMessage(echoSchema), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			w.echo.Add(1)
			if w.gate != nil {
				select {
				case <-w.gate:
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			return "echo:" + string(args), nil
		}},
		{Name: "shout", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(_ context.Context, args json.RawMessage) (string, error) {
			w.shout.Add(1)
			return "SHOUT:" + string(args), nil
		}},
	}
}

func workflowOptions(t *testing.T, root, sid string, model *testkit.FakeModel, defs []tools.Definition, wf agent.WorkflowDefinition, delegable bool) Options {
	t.Helper()
	opts := Options{Workspace: root + "/ws", StateRoot: root + "/state", SessionID: sid, Profile: ProfileMemory, Model: model, Principal: "local", GenerationFingerprint: "workflow-v1", Tools: defs}
	target, err := CompileWorkflowTarget(wf, opts)
	if err != nil {
		t.Fatalf("compile workflow target: %v", err)
	}
	target.Delegable = delegable
	opts.Agents = []agent.AgentDefinition{target}
	return opts
}

func workflowSession(t *testing.T, opts Options, create bool) *AgentSession {
	t.Helper()
	var s *AgentSession
	var err error
	if create {
		s, err = CreateAgentSession(t.Context(), opts)
	} else {
		s, err = OpenAgentSession(t.Context(), opts)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func submitWorkflow(s *AgentSession, name string, content string) (agent.InputReceipt, error) {
	return s.SubmitInput(context.Background(), agent.InputCommand{Kind: "prompt", TargetAgent: name, Content: json.RawMessage(content)})
}

func workflowResult(t *testing.T, s *AgentSession, traceID string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, msg := range s.rt.manager.View().Messages {
		if msg.Scope.TraceID != traceID || msg.Kind != agent.KindCustom || msg.Custom == nil || msg.Custom.CustomType != "workflow_result" {
			continue
		}
		if msg.Source.Kind != agent.SourceTool || !msg.Custom.Display || msg.Custom.Content == nil {
			t.Fatalf("workflow result message %+v", msg)
		}
		var out map[string]any
		if err := json.Unmarshal(msg.Custom.Details, &out); err != nil {
			t.Fatal(err)
		}
		found = append(found, out)
	}
	if len(found) != 1 {
		t.Fatalf("workflow results = %d", len(found))
	}
	return found[0]
}

func TestStandaloneWorkflowRunsNodesWithoutMainModel(t *testing.T) {
	counts := &wfTools{}
	main := testkit.NewFake()
	s := workflowSession(t, workflowOptions(t, agentRoots(t), "wf-standalone", main, counts.definitions(""), triageWorkflow(), false), true)
	// Text-only clients send the input object as JSON text.
	in, err := submitWorkflow(s, "triage", `{"text":"{\"name\":\"ada\",\"score\":80}"}`)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	tr := view.Traces[in.TraceID]
	if tr.State != "completed" || tr.Target.Name != "triage" {
		t.Fatalf("trace %+v", tr)
	}
	if main.Calls() != 0 || counts.echo.Load() != 1 || counts.shout.Load() != 1 {
		t.Fatalf("main=%d echo=%d shout=%d", main.Calls(), counts.echo.Load(), counts.shout.Load())
	}
	out := workflowResult(t, s, in.TraceID)
	if out["name"] != "ada" || !strings.HasPrefix(out["echo"].(string), "echo:") || !strings.Contains(out["echo"].(string), `"g":"hi"`) {
		t.Fatalf("result %+v", out)
	}
	// Node identities are durable and derived from invocation, node and ordinal.
	for _, node := range []string{"t", "big"} {
		id := tr.InvocationID + ":" + node + ":1"
		rec, ok := view.WorkflowNodes[id]
		if !ok || rec.State != "completed" || rec.Kind != "tool" || rec.ToolCallID != id || rec.Result == "" {
			t.Fatalf("node %s record %+v", node, rec)
		}
		call := view.Calls[id]
		if !call.Claimed || call.Observation == nil || call.Observation.Status != "succeeded" || call.Call.ProviderCallID != "" || call.Scope.TurnID != "" {
			t.Fatalf("node %s call %+v", node, call)
		}
		frozen := view.FrozenExecutions["execution:"+id]
		if frozen.Origin != "workflow_node" || frozen.NodeExecutionID != id || frozen.ProviderCallID != "" {
			t.Fatalf("node %s frozen %+v", node, frozen)
		}
	}
	// No model Turn, provider call or tool-result message is fabricated.
	if len(view.Turns) != 0 || len(view.ModelAttempts) != 0 {
		t.Fatalf("turns=%d attempts=%d", len(view.Turns), len(view.ModelAttempts))
	}
	for _, msg := range view.Messages {
		if msg.Kind == agent.KindToolResult || msg.Kind == agent.KindAssistant {
			t.Fatalf("fabricated %s message", msg.Kind)
		}
	}
	if tr.Usage.ToolExecutions != 2 || tr.Usage.LogicalModelCalls != 0 {
		t.Fatalf("usage %+v", tr.Usage)
	}
}

func TestWorkflowUntakenBranchNeverRuns(t *testing.T) {
	counts := &wfTools{}
	s := workflowSession(t, workflowOptions(t, agentRoots(t), "wf-branch", testkit.NewFake(), counts.definitions(""), triageWorkflow(), false), true)
	in, err := submitWorkflow(s, "triage", `{"input":{"name":"bo","score":10}}`)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	if tr := view.Traces[in.TraceID]; tr.State != "completed" {
		t.Fatalf("trace %+v", tr)
	}
	if counts.echo.Load() != 1 || counts.shout.Load() != 0 {
		t.Fatalf("echo=%d shout=%d", counts.echo.Load(), counts.shout.Load())
	}
	if _, ran := view.WorkflowNodes[view.Traces[in.TraceID].InvocationID+":big:1"]; ran {
		t.Fatal("untaken branch node was recorded")
	}
	if out := workflowResult(t, s, in.TraceID); out["name"] != "bo" {
		t.Fatalf("result %+v", out)
	}
}

func TestWorkflowMissingInputRejectedAtAcceptance(t *testing.T) {
	counts := &wfTools{}
	main := testkit.NewFake()
	s := workflowSession(t, workflowOptions(t, agentRoots(t), "wf-missing", main, counts.definitions(""), triageWorkflow(), false), true)
	before := s.rt.manager.View().LastSeq
	for _, content := range []string{`{"input":{"name":"ada"}}`, `{"text":"please triage ada"}`, `{"input":"ada"}`} {
		_, err := submitWorkflow(s, "triage", content)
		if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
			t.Fatalf("%s: %v", content, err)
		}
	}
	if s.rt.manager.View().LastSeq != before || main.Calls() != 0 || counts.echo.Load() != 0 {
		t.Fatalf("rejected input wrote state or ran: seq %d->%d main=%d echo=%d", before, s.rt.manager.View().LastSeq, main.Calls(), counts.echo.Load())
	}
}

func TestWorkflowNodeFailureFailsTraceBeforeDownstream(t *testing.T) {
	counts := &wfTools{}
	// echo requires an integer q; the workflow passes a string, so the
	// controlled pipeline rejects the arguments before the tool starts.
	defs := counts.definitions(`{"type":"object","properties":{"q":{"type":"integer"}}}`)
	s := workflowSession(t, workflowOptions(t, agentRoots(t), "wf-fail", testkit.NewFake(), defs, triageWorkflow(), false), true)
	in, err := submitWorkflow(s, "triage", `{"input":{"name":"ada","score":80}}`)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	tr := view.Traces[in.TraceID]
	if tr.State != "failed" || view.HasUnresolvedEffects() {
		t.Fatalf("trace %+v unresolved=%v", tr, view.HasUnresolvedEffects())
	}
	if counts.echo.Load() != 0 || counts.shout.Load() != 0 {
		t.Fatalf("echo=%d shout=%d", counts.echo.Load(), counts.shout.Load())
	}
	rec := view.WorkflowNodes[tr.InvocationID+":t:1"]
	if rec.State != "failed" || rec.Error == "" {
		t.Fatalf("failed node %+v", rec)
	}
	if call := view.Calls[rec.ToolCallID]; call.Claimed || call.Observation == nil || call.Observation.SideEffect != "none" {
		t.Fatalf("failed call %+v", call)
	}
	for _, msg := range view.Messages {
		if msg.Kind == agent.KindCustom && msg.Custom.CustomType == "workflow_result" {
			t.Fatal("failed workflow produced a result")
		}
	}
}

func TestWorkflowRejectsDirectedFreeText(t *testing.T) {
	counts := &wfTools{gate: make(chan struct{})}
	s := workflowSession(t, workflowOptions(t, agentRoots(t), "wf-steer", testkit.NewFake(), counts.definitions(""), triageWorkflow(), false), true)
	in, err := submitWorkflow(s, "triage", `{"input":{"name":"ada","score":10}}`)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return counts.echo.Load() == 1 })
	before := s.rt.manager.View().LastSeq
	for _, cmd := range []agent.InputCommand{
		{Kind: "steering", TargetTraceID: in.TraceID, Content: json.RawMessage(`{"text":"also check bo"}`)},
		{Kind: "follow_up", TargetTraceID: in.TraceID, Content: json.RawMessage(`{"text":"and then?"}`)},
		{Kind: "chat", Content: json.RawMessage(`{"text":"hello"}`)},
	} {
		_, err := s.SubmitInput(t.Context(), cmd)
		if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeUnsupportedCapability {
			t.Fatalf("%s into workflow: %v", cmd.Kind, err)
		}
	}
	if s.rt.manager.View().LastSeq != before {
		t.Fatal("rejected directed input wrote state")
	}
	close(counts.gate)
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	if tr := s.rt.manager.View().Traces[in.TraceID]; tr.State != "completed" {
		t.Fatalf("trace %+v", tr)
	}
}

func TestDelegateTaskRunsWorkflowAsOneParentResult(t *testing.T) {
	counts := &wfTools{}
	task := `{"name":"ada","score":80}`
	main := testkit.NewFake(delegateCall("triage", task), delegateCall("triage", `{"name":"ada"}`), testkit.Step{Text: "parent done"})
	root := agentRoots(t)
	opts := workflowOptions(t, root, "wf-delegate", main, counts.definitions(""), triageWorkflow(), true)
	s := workflowSession(t, opts, true)
	in := submitPrompt(t, s, "please triage")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	tr := view.Traces[in.TraceID]
	if tr.State != "completed" || main.Calls() != 3 {
		t.Fatalf("trace %+v main=%d", tr, main.Calls())
	}
	if counts.echo.Load() != 1 || counts.shout.Load() != 1 {
		t.Fatalf("echo=%d shout=%d", counts.echo.Load(), counts.shout.Load())
	}
	var results []agent.ToolRecord
	for _, call := range view.Calls {
		if call.Call.Name == delegateToolName {
			results = append(results, call)
		}
	}
	if len(results) != 2 {
		t.Fatalf("delegate calls %d", len(results))
	}
	completed, denied := 0, 0
	for _, call := range results {
		var out delegateResult
		if call.Observation == nil || json.Unmarshal([]byte(call.Observation.Content), &out) != nil {
			t.Fatalf("delegate observation %+v", call.Observation)
		}
		switch out.Status {
		case "completed":
			completed++
			var result map[string]any
			if json.Unmarshal([]byte(out.Result), &result) != nil || result["name"] != "ada" || !strings.HasPrefix(result["echo"].(string), "echo:") {
				t.Fatalf("workflow output in parent result %q", out.Result)
			}
			inv := view.Invocations[out.InvocationID]
			if inv.State != "completed" || inv.ParentCallID != call.Call.CallID || inv.Target.Name != "triage" {
				t.Fatalf("invocation %+v", inv)
			}
			// Child nodes run under the child invocation, not the parent Turn.
			node := view.WorkflowNodes[inv.ID+":t:1"]
			child := view.Calls[node.ToolCallID]
			if node.State != "completed" || child.Scope.InvocationID != inv.ID || child.Scope.TurnID != "" || child.Scope.ParentInvocationID != tr.InvocationID {
				t.Fatalf("child node %+v call %+v", node, child)
			}
		case "denied":
			denied++
			if out.Code != product.CodeInvalidArgument {
				t.Fatalf("missing-field delegation %+v", out)
			}
		default:
			t.Fatalf("delegate result %+v", out)
		}
	}
	if completed != 1 || denied != 1 || len(view.Invocations) != 1 {
		t.Fatalf("completed=%d denied=%d invocations=%d", completed, denied, len(view.Invocations))
	}
	// Workflow results never enter the parent history as their own messages.
	for _, msg := range view.Messages {
		if msg.Kind == agent.KindCustom {
			t.Fatal("delegated workflow wrote a parent history message")
		}
	}
	// Both entry points validate against the one schema capabilities report,
	// and reject the same missing field.
	caps, err := s.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var info agent.AgentInfo
	for _, a := range caps.Agents {
		if a.Name == "triage" {
			info = a
		}
	}
	var delegated agent.TargetAgent
	for _, inv := range view.Invocations {
		delegated = inv.Target
	}
	wf, ok := s.rt.workflowTarget(delegated)
	if !ok || info.Kind != agent.AgentKindWorkflow || !info.Delegable || string(info.InputSchema) != string(wf.Workflow.InputSchema) || string(info.InputSchema) != string(opts.Agents[0].Workflow.InputSchema) {
		t.Fatalf("capability %+v", info)
	}
	_, standaloneErr := submitWorkflow(s, "triage", `{"input":{"name":"ada"}}`)
	if pe, ok := product.AsError(standaloneErr); !ok || pe.Code != product.CodeInvalidArgument {
		t.Fatalf("standalone schema differs from delegation: %v", standaloneErr)
	}
}

func TestWorkflowResumeReusesCompletedNodes(t *testing.T) {
	counts := &wfTools{}
	root := agentRoots(t)
	gate := make(chan struct{})
	defer close(gate)
	first := testkit.NewFake(testkit.Step{Gate: gate})
	opts := workflowOptions(t, root, "wf-resume", first, counts.definitions(""), summarizeWorkflow(), false)
	s := workflowSession(t, opts, true)
	in, err := submitWorkflow(s, "summarize", `{"input":{"name":"ada","score":1}}`)
	if err != nil {
		t.Fatal(err)
	}
	// Interrupt after the tool node completed, while the model node runs.
	waitFor(t, func() bool { return first.Calls() == 1 })
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	second := testkit.NewFake(testkit.Step{Text: "short summary"})
	opts.Model = second
	s = workflowSession(t, opts, false)
	view := s.rt.manager.View()
	tr := view.Traces[in.TraceID]
	if tr.State != "paused" || !tr.ExecutionStopped {
		t.Fatalf("interrupted trace %+v", tr)
	}
	if rec := view.WorkflowNodes[tr.InvocationID+":t:1"]; rec.State != "completed" {
		t.Fatalf("tool node %+v", rec)
	}
	if rec := view.WorkflowNodes[tr.InvocationID+":m:1"]; rec.State != "accepted" {
		t.Fatalf("model node %+v", rec)
	}
	snap, err := s.Snapshot(t.Context())
	if err != nil || !snap.Resume[in.TraceID].CanResume {
		t.Fatalf("resume eligibility %+v %v", snap.Resume[in.TraceID], err)
	}
	if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: in.TraceID, ExpectedRevision: view.LastSeq}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view = s.rt.manager.View()
	// Resume keeps the original workflow invocation, so node identities match.
	if resumed := view.Traces[in.TraceID]; resumed.State != "completed" || resumed.InvocationID != tr.InvocationID {
		t.Fatalf("resumed trace %+v", resumed)
	}
	// The completed tool node executed exactly once across both processes.
	if counts.echo.Load() != 1 || second.Calls() != 1 {
		t.Fatalf("echo=%d model=%d", counts.echo.Load(), second.Calls())
	}
	if out := workflowResult(t, s, in.TraceID); out["summary"] != "short summary" {
		t.Fatalf("result %+v", out)
	}
	if rec := view.WorkflowNodes[tr.InvocationID+":m:1"]; rec.State != "completed" || rec.Result != "short summary" || rec.ModelCalls != 1 {
		t.Fatalf("model node %+v", rec)
	}
}

// s -> first(shout) -> t(echo, needs approval) -> e.
func approvalWorkflow() agent.WorkflowDefinition {
	return agent.WorkflowDefinition{Name: "gated", Version: "w1", Source: "test", FormatVersion: agent.WorkflowFormatV1,
		InputSchema: json.RawMessage(wfInputSchema),
		Nodes: []agent.WorkflowNode{
			{ID: "s", Type: agent.WorkflowNodeStart},
			{ID: "first", Type: agent.WorkflowNodeTool, Tool: "shout", Inputs: map[string]agent.WorkflowValue{"text": wfOutput("s", "name")}},
			{ID: "t", Type: agent.WorkflowNodeTool, Tool: "echo", Inputs: map[string]agent.WorkflowValue{"q": wfOutput("first", "result")}},
			{ID: "e", Type: agent.WorkflowNodeEnd, Inputs: map[string]agent.WorkflowValue{"echo": wfOutput("t", "result")}},
		},
		Edges: []agent.WorkflowEdge{{From: "s", To: "first"}, {From: "first", To: "t"}, {From: "t", To: "e"}},
	}
}

func approvalWorkflowOptions(t *testing.T, root, sid string, counts *wfTools) Options {
	t.Helper()
	defs := counts.definitions("")
	defs[0].Execution.RequestedGrantRef = "one-operation" // echo needs a one-time approval
	return workflowOptions(t, root, sid, testkit.NewFake(), defs, approvalWorkflow(), false)
}

// waitWorkflowApproval waits until the gated node waits in a stopped segment
// and returns its single answerable runtime interaction.
func waitWorkflowApproval(t *testing.T, s *AgentSession, traceID string) (state.WorkflowNodeRun, state.Interaction) {
	t.Helper()
	waitFor(t, func() bool {
		tr := s.rt.manager.View().Traces[traceID]
		return tr.State == "paused" && tr.ExecutionStopped || terminal(tr.State)
	})
	view := s.rt.manager.View()
	tr := view.Traces[traceID]
	node := view.WorkflowNodes[tr.InvocationID+":t:1"]
	snap := approvalSnapshot(t, s)
	var pending []state.Interaction
	for _, in := range snap.Interactions {
		if in.CallID == node.ToolCallID && in.State == "ready" {
			pending = append(pending, in)
		}
	}
	if tr.State != "paused" || tr.Settled || node.State != "waiting" || !node.ApprovalWait || len(pending) != 1 {
		t.Fatalf("trace=%+v node=%+v interactions=%+v", tr, node, snap.Interactions)
	}
	call := view.Calls[node.ToolCallID]
	if call.Claimed || call.Observation != nil {
		t.Fatalf("waiting call manufactured a result: %+v", call)
	}
	return node, pending[0]
}

func respond(t *testing.T, s *AgentSession, interactionID, decision string) error {
	t.Helper()
	_, err := s.RespondInteraction(t.Context(), InteractionResponse{InteractionID: interactionID, Decision: decision, ExpectedRevision: s.rt.manager.View().LastSeq})
	return err
}

func resumeWorkflowTrace(t *testing.T, s *AgentSession, traceID string) {
	t.Helper()
	if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: traceID, ExpectedRevision: s.rt.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if pe, ok := product.AsError(err); !ok || pe.Code != code {
		t.Fatalf("error %v, want %s", err, code)
	}
}

func TestWorkflowToolApprovalAllowedRunsSameCallOnceAfterResume(t *testing.T) {
	counts := &wfTools{}
	s := workflowSession(t, approvalWorkflowOptions(t, agentRoots(t), "wf-approve", counts), true)
	in, err := submitWorkflow(s, "gated", `{"input":{"name":"ada","score":1}}`)
	if err != nil {
		t.Fatal(err)
	}
	node, question := waitWorkflowApproval(t, s, in.TraceID)
	if counts.shout.Load() != 1 || counts.echo.Load() != 0 || node.ToolCallID != node.ID || node.Attempt != 1 {
		t.Fatalf("before answer: shout=%d echo=%d node=%+v", counts.shout.Load(), counts.echo.Load(), node)
	}
	// The snapshot exposes durable node records by nodeExecutionId.
	snap := approvalSnapshot(t, s)
	if got := snap.WorkflowNodes[node.ID]; got.State != "waiting" || got.Kind != "tool" || snap.WorkflowNodes[s.rt.manager.View().Traces[in.TraceID].InvocationID+":first:1"].State != "completed" {
		t.Fatalf("snapshot nodes %+v", snap.WorkflowNodes)
	}
	if err := respond(t, s, question.ID, "allowed-once"); err != nil {
		t.Fatal(err)
	}
	// A decision is recorded, never applied without Resume.
	if tr := s.rt.manager.View().Traces[in.TraceID]; tr.State != "paused" || counts.echo.Load() != 0 {
		t.Fatalf("answer executed work: trace=%s echo=%d", tr.State, counts.echo.Load())
	}
	requireCode(t, respond(t, s, question.ID, "allowed-once"), product.CodeStateConflict)
	resumeWorkflowTrace(t, s, in.TraceID)
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	tr := view.Traces[in.TraceID]
	done := view.WorkflowNodes[node.ID]
	call := view.Calls[node.ID]
	if tr.State != "completed" || counts.echo.Load() != 1 || counts.shout.Load() != 1 || tr.Usage.ToolExecutions != 2 {
		t.Fatalf("trace=%s echo=%d shout=%d usage=%+v", tr.State, counts.echo.Load(), counts.shout.Load(), tr.Usage)
	}
	// The waiting node executed its original toolCallId, not a new attempt.
	if done.State != "completed" || done.Attempt != 1 || done.ToolCallID != node.ID || !call.Claimed || call.Observation == nil || call.Observation.Status != "succeeded" {
		t.Fatalf("node=%+v call=%+v", done, call)
	}
	if _, retried := view.Calls[node.ID+"#2"]; retried {
		t.Fatal("approved node registered a second attempt")
	}
	if out := workflowResult(t, s, in.TraceID); !strings.HasPrefix(out["echo"].(string), "echo:") {
		t.Fatalf("result %+v", out)
	}
	if snap := approvalSnapshot(t, s); snap.Interactions[question.ID].State != "claimed" {
		t.Fatalf("approval not consumed: %+v", snap.Interactions[question.ID])
	}
}

func TestWorkflowToolApprovalRejectedNeverRuns(t *testing.T) {
	counts := &wfTools{}
	s := workflowSession(t, approvalWorkflowOptions(t, agentRoots(t), "wf-reject", counts), true)
	in, err := submitWorkflow(s, "gated", `{"input":{"name":"ada","score":1}}`)
	if err != nil {
		t.Fatal(err)
	}
	node, question := waitWorkflowApproval(t, s, in.TraceID)
	if err := respond(t, s, question.ID, "rejected"); err != nil {
		t.Fatal(err)
	}
	if tr := s.rt.manager.View().Traces[in.TraceID]; tr.State != "paused" {
		t.Fatalf("rejection changed trace before Resume: %s", tr.State)
	}
	resumeWorkflowTrace(t, s, in.TraceID)
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	tr := view.Traces[in.TraceID]
	rec := view.WorkflowNodes[node.ID]
	call := view.Calls[node.ToolCallID]
	if tr.State != "failed" || rec.State != "failed" || counts.echo.Load() != 0 || counts.shout.Load() != 1 || call.Claimed || call.Observation == nil || call.Observation.Status != "denied" || call.Observation.SideEffect != "none" || view.HasUnresolvedEffects() || tr.Usage.ToolExecutions != 1 {
		t.Fatalf("trace=%s node=%+v call=%+v echo=%d shout=%d", tr.State, rec, call, counts.echo.Load(), counts.shout.Load())
	}
}

// Approvals live only in the process instance: a reopened session cannot
// answer the old question, and Resume asks again instead of auto-approving.
func TestWorkflowToolApprovalReopenAsksAgain(t *testing.T) {
	counts := &wfTools{}
	opts := approvalWorkflowOptions(t, agentRoots(t), "wf-reopen", counts)
	s := workflowSession(t, opts, true)
	in, err := submitWorkflow(s, "gated", `{"input":{"name":"ada","score":1}}`)
	if err != nil {
		t.Fatal(err)
	}
	_, old := waitWorkflowApproval(t, s, in.TraceID)
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	s = workflowSession(t, opts, false)
	if snap := approvalSnapshot(t, s); len(snap.Interactions) != 0 || snap.WorkflowNodes[s.rt.manager.View().Traces[in.TraceID].InvocationID+":t:1"].State != "waiting" {
		t.Fatalf("reopened snapshot interactions=%+v nodes=%+v", snap.Interactions, snap.WorkflowNodes)
	}
	requireCode(t, respond(t, s, old.ID, "allowed-once"), product.CodeNotFound)
	resumeWorkflowTrace(t, s, in.TraceID)
	node, again := waitWorkflowApproval(t, s, in.TraceID)
	if again.ID == old.ID || counts.echo.Load() != 0 || counts.shout.Load() != 1 || node.Attempt != 1 {
		t.Fatalf("reopen resume: new=%s old=%s echo=%d shout=%d node=%+v", again.ID, old.ID, counts.echo.Load(), counts.shout.Load(), node)
	}
	if err := respond(t, s, again.ID, "allowed-once"); err != nil {
		t.Fatal(err)
	}
	resumeWorkflowTrace(t, s, in.TraceID)
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	tr := s.rt.manager.View().Traces[in.TraceID]
	if tr.State != "completed" || counts.echo.Load() != 1 || counts.shout.Load() != 1 || tr.Usage.ToolExecutions != 2 {
		t.Fatalf("trace=%s echo=%d shout=%d usage=%+v", tr.State, counts.echo.Load(), counts.shout.Load(), tr.Usage)
	}
}

func TestWorkflowPauseStopsAtNodeBoundary(t *testing.T) {
	counts := &wfTools{gate: make(chan struct{})}
	s := workflowSession(t, workflowOptions(t, agentRoots(t), "wf-pause", testkit.NewFake(), counts.definitions(""), triageWorkflow(), false), true)
	in, err := submitWorkflow(s, "triage", `{"input":{"name":"ada","score":80}}`)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return counts.echo.Load() == 1 })
	paused := make(chan error, 1)
	go func() {
		_, err := s.Pause(context.Background(), in.TraceID)
		paused <- err
	}()
	waitFor(t, func() bool {
		v, err := s.rt.call(t.Context(), func(rt *runtime) (any, error) { return rt.active != nil && rt.active.pauseID != "", nil })
		return err == nil && v.(bool)
	})
	close(counts.gate) // the running node finishes; the next one must not start
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	view := s.rt.manager.View()
	tr := view.Traces[in.TraceID]
	if tr.State != "paused" || !tr.ExecutionStopped || tr.Settled || counts.echo.Load() != 1 || counts.shout.Load() != 0 {
		t.Fatalf("trace=%+v echo=%d shout=%d", tr, counts.echo.Load(), counts.shout.Load())
	}
	if rec := view.WorkflowNodes[tr.InvocationID+":t:1"]; rec.State != "completed" {
		t.Fatalf("running node did not finish: %+v", rec)
	}
	if _, started := view.WorkflowNodes[tr.InvocationID+":big:1"]; started {
		t.Fatal("next node started after pause")
	}
	for _, op := range view.Operations {
		if op.Kind == "pause" && op.State != "completed" {
			t.Fatalf("pause operation %+v", op)
		}
	}
	resumeWorkflowTrace(t, s, in.TraceID)
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	tr = s.rt.manager.View().Traces[in.TraceID]
	if tr.State != "completed" || counts.echo.Load() != 1 || counts.shout.Load() != 1 || tr.Usage.ToolExecutions != 2 {
		t.Fatalf("resumed trace=%s echo=%d shout=%d usage=%+v", tr.State, counts.echo.Load(), counts.shout.Load(), tr.Usage)
	}
}

func TestWorkflowCancelLeavesNoPendingCall(t *testing.T) {
	counts := &wfTools{}
	s := workflowSession(t, approvalWorkflowOptions(t, agentRoots(t), "wf-cancel", counts), true)
	in, err := submitWorkflow(s, "gated", `{"input":{"name":"ada","score":1}}`)
	if err != nil {
		t.Fatal(err)
	}
	node, question := waitWorkflowApproval(t, s, in.TraceID)
	if _, err := s.CancelTrace(t.Context(), CancelTraceRequest{TraceID: in.TraceID, IdempotencyKey: "cancel-1"}); err != nil {
		t.Fatal(err)
	}
	view := s.rt.manager.View()
	if tr := view.Traces[in.TraceID]; tr.State != "cancelled" || !tr.Settled {
		t.Fatalf("trace %+v", tr)
	}
	for id, call := range view.Calls {
		if call.Scope.TraceID == in.TraceID && call.Observation == nil {
			t.Fatalf("call %s left without observation", id)
		}
	}
	if call := view.Calls[node.ToolCallID]; call.Claimed || call.Observation.Status != "skipped" || call.Observation.SideEffect != "none" || view.HasUnresolvedEffects() {
		t.Fatalf("cancelled waiting call %+v", call)
	}
	if counts.echo.Load() != 0 {
		t.Fatalf("echo=%d", counts.echo.Load())
	}
	requireCode(t, respond(t, s, question.ID, "allowed-once"), product.CodeStateConflict)
}

// A workflow call has no model attempt, so the legacy model rule alone would
// accept it. The workflow rule is checked first and closes it once the node
// is terminal.
func TestWorkflowCallAcceptanceFollowsItsNode(t *testing.T) {
	counts := &wfTools{}
	s := workflowSession(t, workflowOptions(t, agentRoots(t), "wf-lookup", testkit.NewFake(), counts.definitions(""), triageWorkflow(), false), true)
	in, err := submitWorkflow(s, "triage", `{"input":{"name":"ada","score":10}}`)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	id := view.Traces[in.TraceID].InvocationID + ":t:1"
	call := view.Calls[id]
	if !acceptedAttemptForCall(view, call) {
		t.Fatal("fixture: legacy acceptance precondition changed")
	}
	if s.rt.acceptedCall(view, call) {
		t.Fatal("completed workflow node call still accepted for execution")
	}
	// A call ID that only resembles an attempt suffix is not workflow-bound.
	if _, bound := view.WorkflowNodeForCall(id + "#x"); bound {
		t.Fatal("non-numeric attempt suffix bound to a workflow node")
	}
	if node, bound := view.WorkflowNodeForCall(id + "#2"); !bound || node.ID != id {
		t.Fatal("attempt suffix did not resolve to its node")
	}
}
