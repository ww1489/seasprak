package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
)

func TestWorkflowUnknownHoldBelongsToRealRunRoot(t *testing.T) {
	scope := agent.ExecutionScope{WorkflowRunID: "same-id", InvocationID: "i", NodeExecutionID: "n", ExecutionID: "e", Generation: "g"}
	probe := &workflowProbe{call: agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: "call", Name: "effect", Arguments: `{}`, Generation: "g", Hash: callHash("effect", "v1", `{}`, "g")}}}
	runs := 0
	def := Definition{Name: "effect", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: ExecutionDescription{Effect: "write", Resources: []agent.ExecutionResource{{Identity: "resource"}}}, Run: func(context.Context, json.RawMessage) (string, error) {
		runs++
		return "", errors.New("synthetic execution fault")
	}}
	scheduler := NewResourceScheduler()
	exec, err := NewExecutor("g", []Definition{def}, registeredWorkflowProbe{probe}, probe, agent.NewBudget(config.DefaultLimits()), WithResourceScheduler(scheduler))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.RunWorkflowNode(t.Context(), scope, "n", "call", "effect", `{}`)
	if err != nil || out.SideEffect != "unknown" || runs != 1 {
		t.Fatalf("out=%+v err=%v runs=%d", out, err, runs)
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if _, ok := scheduler.holds[ResourceHoldID("workflow:same-id", "call")]; !ok {
		t.Fatal("unknown hold used empty/fake session instead of workflow root")
	}
	if _, ok := scheduler.holds[ResourceHoldID("same-id", "call")]; ok {
		t.Fatal("workflow hold collided with code owner")
	}
}
