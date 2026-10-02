package workflowagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
)

type workflowProcess struct{ calls atomic.Int32 }

func (p *workflowProcess) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return agent.BackendCapabilities{BackendID: "workflow-process-fixture", Version: "v1", EnvironmentID: "memory", SupportedModes: []string{"read-only", "workspace-write", "danger-full-access"}, Enforcement: "partial", RuntimeDataWriteProtected: true}, nil
}
func (p *workflowProcess) Execute(ctx context.Context, req agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.calls.Add(1)
	return agent.ProcessObservation{Started: true, Terminated: true, SideEffect: "none", Content: strings.Repeat("output\n", 16000)}, nil
}
func (*workflowProcess) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}
func TestWorkflowControlledProcessProjectionKeepsExecutionAndTruncation(t *testing.T) {
	var count atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &count)
	process := &workflowProcess{}
	opts.Tools[0].Run = nil
	opts.Tools[0].Execution = tools.ExecutionDescription{BackendID: "process-operations", Effect: "read", Argv: []string{"synthetic"}}
	opts.Operations.Process = process
	w := newWorkflow(t, opts)
	submit(t, w)
	s := waitStopped(t, w)
	if s.State != "completed" || process.calls.Load() != 1 {
		t.Fatalf("controlled process state=%s calls=%d", s.State, process.calls.Load())
	}
	w.mu.Lock()
	for _, c := range w.state.Calls {
		if c.Projection == nil || !c.Observation.Executed || !c.Observation.Terminated || !c.Projection.Truncated {
			t.Errorf("projection missing or facts lost: %+v", c)
		}
	}
	w.mu.Unlock()
	var output struct{ Result string }
	if json.Unmarshal(s.Result, &output) != nil || !strings.Contains(output.Result, `"truncated":true`) {
		t.Errorf("result lost bounded envelope %q", output.Result)
	}
}
