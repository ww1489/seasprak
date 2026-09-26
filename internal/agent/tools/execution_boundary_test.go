package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestExecutorCopiesBeforeCallHooks(t *testing.T) {
	var runs int
	hooks := []func(context.Context, agent.FrozenExecution) error{
		func(context.Context, agent.FrozenExecution) error {
			return product.NewError(product.CodePermissionDenied, "blocked")
		},
	}
	def := addDef(func(context.Context, json.RawMessage) (string, error) {
		runs++
		return "done", nil
	})
	def.BeforeCall = hooks
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	executor, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	// Changing the registration input must not replace the installed safety hook.
	hooks[0] = func(context.Context, agent.FrozenExecution) error { return nil }
	out, err := executor.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodePermissionDenied || runs != 0 || out.Executed || hasIntent(sink) {
		t.Fatalf("mutable hook input changed registered behavior: err=%v out=%+v runs=%d", err, out, runs)
	}
}

type runningProcessObservation struct{ starts int }

func (p *runningProcessObservation) Execute(ctx context.Context, request agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := request.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.starts++
	// No effects have been observed yet; this is not a terminal no-effects proof.
	return agent.ProcessObservation{Started: true, Terminated: false, SideEffect: "none"}, nil
}

func (*runningProcessObservation) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{}, nil
}

func TestUnterminatedProcessRetainsResources(t *testing.T) {
	proc := &runningProcessObservation{}
	scheduler := NewResourceScheduler()
	record := accepted(`{"n":1}`)
	record.Scope = agent.ExecutionScope{SessionID: "session", InvocationID: "invocation", TurnID: "turn"}
	sink := &recordSink{found: true, rec: record}
	def := addDef(nil)
	def.Execution = ExecutionDescription{BackendID: "process-operations", Effect: "write", Resources: []agent.ExecutionResource{{Identity: "file"}}, Argv: []string{"fixture"}}
	executor, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Process: proc}), WithResourceScheduler(scheduler), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := executor.Run(t.Context(), record.Scope, "prov-1", "add", `{"n":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if proc.starts != 1 || !hasIntent(sink) {
		t.Fatalf("fixture did not reach the claimed backend: starts=%d", proc.starts)
	}
	if !scheduler.HasHold(ResourceHoldID("session", "prod-1")) {
		t.Fatalf("running process released resources: out=%+v", out)
	}
	if out.Status == "succeeded" || !hasObservation(t, sink, "outcome_unknown") {
		t.Fatalf("running process was finalized as a successful tool: out=%+v", out)
	}
}
