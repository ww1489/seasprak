package tools

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

// workflowProbe is a session double. With registered=false it has no
// WorkflowToolSource, so the executor cannot find a registered node call.
type workflowProbe struct {
	call                  agent.ToolRecord
	frozen                agent.FrozenExecution
	facts                 []string
	authorized, selection int
}

func (p *workflowProbe) CommitFact(_ context.Context, _ agent.ExecutionScope, fact agent.Fact) error {
	p.facts = append(p.facts, fact.Kind)
	switch fact.Kind {
	case "tool_frozen":
		return json.Unmarshal(fact.Payload, &p.frozen)
	case "tool_intent":
		p.call.Claimed = true
	case "tool_observation":
		var rec agent.ToolRecord
		if err := json.Unmarshal(fact.Payload, &rec); err != nil {
			return err
		}
		p.call.Observation = rec.Observation
	}
	return nil
}
func (p *workflowProbe) ExecutionPolicyRef(context.Context, agent.ExecutionScope) (string, error) {
	return "workflow-test-policy", nil
}
func (p *workflowProbe) Authorize(context.Context, agent.FrozenCall) (agent.Decision, error) {
	p.authorized++
	return agent.DecisionAllow, nil
}
func (p *workflowProbe) ToolSelected(context.Context, agent.ExecutionScope, string) (bool, error) {
	p.selection++
	return false, nil
}

type registeredWorkflowProbe struct{ *workflowProbe }

func (p registeredWorkflowProbe) LookupWorkflowTool(_ context.Context, _ agent.ExecutionScope, callID string) (agent.ToolRecord, error) {
	if callID != p.call.Call.CallID {
		return agent.ToolRecord{}, product.NewError(product.CodePermissionDenied, "not registered")
	}
	return p.call, nil
}

func TestRunWorkflowNodeUsesControlledPipeline(t *testing.T) {
	var runs atomic.Int32
	def := Definition{Name: "echo", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		return "ok", nil
	}}
	scope := agent.ExecutionScope{SessionID: "s", TraceID: "t", InvocationID: "i", ExecutionID: "e", Generation: "g"}
	const node, callID, args = "i:tool:1", "i:tool:1", `{"q":"x"}`

	unregistered := &workflowProbe{}
	budget := agent.NewBudget(config.DefaultLimits())
	exec, err := NewExecutor("g", []Definition{def}, unregistered, unregistered, budget, WithResourceScheduler(NewResourceScheduler()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.RunWorkflowNode(t.Context(), scope, node, callID, "echo", args); err == nil {
		t.Fatal("unregistered workflow call ran")
	}
	if runs.Load() != 0 || len(unregistered.facts) != 0 || budget.Snapshot().ToolExecutions != 0 {
		t.Fatalf("unregistered call committed facts %v runs=%d", unregistered.facts, runs.Load())
	}

	probe := &workflowProbe{call: agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: callID, Name: "echo", Arguments: args, Generation: "g", Hash: callHash("echo", "1", args, "g")}}}
	budget = agent.NewBudget(config.DefaultLimits())
	exec, err = NewExecutor("g", []Definition{def}, registeredWorkflowProbe{probe}, probe, budget, WithResourceScheduler(NewResourceScheduler()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.RunWorkflowNode(t.Context(), scope, node, callID, "echo", args)
	if err != nil || out.Status != "succeeded" || out.Content != "ok" {
		t.Fatalf("workflow node result %+v %v", out, err)
	}
	if runs.Load() != 1 || probe.selection != 0 || probe.authorized != 1 || !probe.call.Claimed || budget.Snapshot().ToolExecutions != 1 {
		t.Fatalf("runs=%d selection=%d authorized=%d claimed=%v budget=%+v", runs.Load(), probe.selection, probe.authorized, probe.call.Claimed, budget.Snapshot())
	}
	if probe.frozen.Origin != "workflow_node" || probe.frozen.NodeExecutionID != node || probe.frozen.ProviderCallID != "" || probe.frozen.CallID != callID {
		t.Fatalf("frozen %+v", probe.frozen)
	}
	// A completed call returns its saved observation without running again.
	again, err := exec.RunWorkflowNode(t.Context(), scope, node, callID, "echo", args)
	if err != nil || again.Content != "ok" || runs.Load() != 1 {
		t.Fatalf("replayed node %+v %v runs=%d", again, err, runs.Load())
	}
	// Model calls keep the selection check.
	if _, err := exec.Run(t.Context(), scope, "model-call", "echo", args); err != nil {
		t.Fatal(err)
	}
	if probe.selection != 1 || runs.Load() != 1 {
		t.Fatalf("model origin skipped selection: selection=%d runs=%d", probe.selection, runs.Load())
	}
}
