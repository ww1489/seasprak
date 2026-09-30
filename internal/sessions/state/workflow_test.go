package state_test

import (
	"context"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func TestWorkflowNodeRecordLifecycleAndReopen(t *testing.T) {
	ctx := context.Background()
	m, s := fixture(t)
	r := accept(t, m, "wf", `{"input":{}}`)
	if err := m.SetTraceState(ctx, r.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	inv := m.View().Traces[r.TraceID].InvocationID
	id := inv + ":t:1"
	scope := agent.ExecutionScope{SessionID: "session", TraceID: r.TraceID, InvocationID: inv, ExecutionID: "e", Generation: "g"}
	node := state.WorkflowNodeRun{ID: id, TraceID: r.TraceID, InvocationID: inv, NodeID: "t", Kind: "tool", State: "accepted", ToolCallID: id, Attempt: 1}
	call := agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: id, Name: "echo", Arguments: `{}`, Generation: "g"}}

	// A tool node needs its call; a model-bound call identity is rejected.
	if err := m.BeginWorkflowNode(ctx, node, nil); err == nil {
		t.Fatal("tool node began without its call")
	}
	forged := call
	forged.Call.ProviderCallID = "provider"
	if err := m.BeginWorkflowNode(ctx, node, &forged); err == nil {
		t.Fatal("workflow call with a provider identity was registered")
	}
	turnScoped := call
	turnScoped.Scope.TurnID = "turn"
	if err := m.BeginWorkflowNode(ctx, node, &turnScoped); err == nil {
		t.Fatal("workflow call borrowed a Turn")
	}
	before := m.View().LastSeq
	if err := m.BeginWorkflowNode(ctx, node, &call); err != nil {
		t.Fatal(err)
	}
	v := m.View()
	if v.LastSeq != before+1 || v.WorkflowNodes[id].State != "accepted" || v.Calls[id].Call.CallID != id {
		t.Fatalf("node and call were not committed together: %+v", v.WorkflowNodes[id])
	}
	if got, ok := v.WorkflowNodeForCall(id); !ok || got.ID != id {
		t.Fatal("registered call does not resolve to its node")
	}
	// Re-beginning without a retryable previous attempt is a conflict.
	retry := node
	retry.Attempt, retry.ToolCallID = 2, id+"#2"
	second := call
	second.Call.CallID = retry.ToolCallID
	if err := m.BeginWorkflowNode(ctx, retry, &second); err == nil {
		t.Fatal("new attempt started while the first call was pending")
	}
	done := node
	done.State, done.Result = "completed", "ok"
	if err := m.FinishWorkflowNode(ctx, done); err != nil {
		t.Fatal(err)
	}
	failed := done
	failed.State = "failed"
	if err := m.FinishWorkflowNode(ctx, failed); err == nil {
		t.Fatal("terminal node changed")
	}
	reopened, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.View().WorkflowNodes[id]; got != done {
		t.Fatalf("replayed node %+v", got)
	}
}

// A node may only wait for an original pending call frozen with a requested
// grant, and a workflow approval claim requires a node that waited.
func TestWorkflowStopAndApprovalClaimPreconditions(t *testing.T) {
	ctx := context.Background()
	m, _ := fixture(t)
	r := accept(t, m, "wf", `{"input":{}}`)
	if err := m.SetTraceState(ctx, r.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	inv := m.View().Traces[r.TraceID].InvocationID
	id := inv + ":t:1"
	scope := agent.ExecutionScope{SessionID: "session", TraceID: r.TraceID, InvocationID: inv, ExecutionID: "e", Generation: "g"}
	node := state.WorkflowNodeRun{ID: id, TraceID: r.TraceID, InvocationID: inv, NodeID: "t", Kind: "tool", State: "accepted", ToolCallID: id, Attempt: 1}
	call := agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: id, Name: "echo", Arguments: `{}`, Generation: "g"}}
	if err := m.BeginWorkflowNode(ctx, node, &call); err != nil {
		t.Fatal(err)
	}
	before := m.View().LastSeq
	// No frozen descriptor with a requested grant: the node cannot wait.
	if err := m.CommitWorkflowStop(ctx, r.TraceID, "e", "", id); err == nil {
		t.Fatal("node waited without a frozen approval request")
	}
	// The node never waited, so no runtime approval may claim its call.
	if err := m.ClaimWorkflowApprovedTool(ctx, call.Call, agent.Usage{ToolExecutions: 1}, "e2"); err == nil {
		t.Fatal("workflow approval claim accepted a node that never waited")
	}
	if m.View().LastSeq != before || m.View().Calls[id].Claimed {
		t.Fatal("rejected transitions wrote state")
	}
	// A plain boundary stop pauses with the stopped proof and keeps the node.
	if err := m.CommitWorkflowStop(ctx, r.TraceID, "e", "", ""); err != nil {
		t.Fatal(err)
	}
	tr := m.View().Traces[r.TraceID]
	if tr.State != "paused" || !tr.ExecutionStopped || tr.Settled || tr.ExecutionID != "e" || m.View().WorkflowNodes[id].State != "accepted" {
		t.Fatalf("stopped trace %+v node %+v", tr, m.View().WorkflowNodes[id])
	}
}
