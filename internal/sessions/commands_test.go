package sessions

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
)

func builtinDefinitionForSession(t *testing.T, name string) tools.Definition {
	t.Helper()
	for _, def := range tools.NewBuiltinDefinitions(tools.BuiltinOptions{}) {
		if def.Name == name {
			return def
		}
	}
	t.Fatalf("builtin %q missing", name)
	return tools.Definition{}
}

type commandProcessProbe struct{ calls atomic.Int32 }

func (p *commandProcessProbe) Execute(ctx context.Context, req agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.calls.Add(1)
	return agent.ProcessObservation{Started: true, Terminated: true, ExitCode: 0, Content: "hi", SideEffect: "none"}, nil
}
func (*commandProcessProbe) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}
