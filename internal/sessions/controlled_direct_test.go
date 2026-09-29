package sessions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/testkit"
)

func onlyControlledCall(t *testing.T, s *AgentSession) agent.ToolRecord {
	t.Helper()
	calls := s.rt.manager.View().Calls
	if len(calls) != 1 {
		t.Fatalf("controlled calls=%d", len(calls))
	}
	for _, call := range calls {
		return call
	}
	return agent.ToolRecord{}
}

func capabilityApprovalFixture(t *testing.T, disk bool) (*AgentSession, Options, *commandProcessProbe, string, string) {
	t.Helper()
	process := &commandProcessProbe{}
	def := builtinDefinitionForSession(t, "execute")
	def.Execution.RequestedGrantRef = "requires-approval"
	root := "memory"
	if disk {
		root = t.TempDir()
	}
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: root, Profile: ProfileMemory, Principal: "host", GenerationFingerprint: "capability-model-approval",
		Model: versionedPauseModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "execute", Name: "execute", Arguments: `{"argv":["echo","hi"],"cwd":"workspace"}`}}}, testkit.Step{Text: "done"})},
		Tools: []tools.Definition{def}, Operations: tools.Operations{Process: process}, ResourceScheduler: tools.NewResourceScheduler()}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	receipt := submitOutput(t, s)
	waitResumeCondition(t, func() bool {
		tr := s.rt.manager.View().Traces[receipt.TraceID]
		return tr.State == "paused" || terminal(tr.State)
	})
	if s.rt.manager.View().Traces[receipt.TraceID].State != "paused" {
		t.Fatal("model tool did not wait for approval")
	}
	return s, opts, process, onlyControlledCall(t, s).Call.CallID, receipt.TraceID
}

// This fixture calls the retained controlled Executor directly. It deliberately
// has no public Session command API and records the actual pipeline facts.
type controlledDirectProbe struct {
	call                  agent.ToolRecord
	frozen                agent.FrozenExecution
	approvals, selections int
}

func (p *controlledDirectProbe) CommitFact(_ context.Context, scope agent.ExecutionScope, fact agent.Fact) error {
	switch fact.Kind {
	case "tool_frozen":
		return json.Unmarshal(fact.Payload, &p.frozen)
	case "tool_intent":
		p.call.Scope = scope
		p.call.Claimed = true
		return json.Unmarshal(fact.Payload, &p.call.Call)
	case "tool_observation":
		var call agent.ToolRecord
		if err := json.Unmarshal(fact.Payload, &call); err != nil {
			return err
		}
		p.call.Observation = call.Observation
	}
	return nil
}
func (p *controlledDirectProbe) ExecutionPolicyRef(context.Context, agent.ExecutionScope) (string, error) {
	return "controlled-test-policy", nil
}
func (p *controlledDirectProbe) Authorize(context.Context, agent.FrozenCall) (agent.Decision, error) {
	p.approvals++
	return agent.DecisionAllow, nil
}
func (p *controlledDirectProbe) ToolSelected(context.Context, agent.ExecutionScope, string) (bool, error) {
	p.selections++
	return false, nil
}
func runControlledDirect(t *testing.T, def tools.Definition) agent.ToolRecord {
	t.Helper()
	probe := &controlledDirectProbe{}
	budget := agent.NewBudget(config.DefaultLimits())
	executor, err := tools.NewExecutor("controlled-direct-test", []tools.Definition{def}, probe, probe, budget, tools.WithResourceScheduler(tools.NewResourceScheduler()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := executor.RunDirect(t.Context(), agent.ExecutionScope{SessionID: "controlled-direct", Generation: "controlled-direct-test"}, agent.MustID(), def.Name, `{}`)
	if err != nil || out.Status != "succeeded" {
		t.Fatalf("controlled direct result=%+v err=%v", out, err)
	}
	if probe.approvals == 0 || probe.selections != 0 || probe.frozen.Origin != "direct" || !probe.call.Claimed || budget.Snapshot().ToolExecutions != 1 {
		t.Fatal("controlled direct skipped authorization, claim, budget, or invented model selection")
	}
	return probe.call
}
