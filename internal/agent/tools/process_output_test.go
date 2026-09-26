package tools

import (
	"context"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
)

type referenceOnlyProcess struct{ calls int }

func (p *referenceOnlyProcess) Execute(ctx context.Context, request agent.AuthorizedProcess, sink agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := request.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.calls++
	if err := sink.WriteProgress(ctx, agent.ProcessProgress{Stream: "stderr", ContentRef: "not-public-log-ref", Sequence: 1000}); err != nil {
		return agent.ProcessObservation{}, err
	}
	return agent.ProcessObservation{Started: true, Terminated: true, Content: "final", SideEffect: "none"}, nil
}
func (*referenceOnlyProcess) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{}, nil
}

func TestProcessReferenceWithoutTextNeverPublishesOutput(t *testing.T) {
	process := &referenceOnlyProcess{}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	def := addDef(nil)
	def.Execution = ExecutionDescription{BackendID: "process-operations", Effect: "read", Argv: []string{"fixture"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Process: process}))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
	if err != nil || out.Content != "final" || process.calls != 1 || len(outputFacts(t, sink)) != 0 {
		t.Fatalf("output ref was published: out=%+v err=%v calls=%d", out, err, process.calls)
	}
}
