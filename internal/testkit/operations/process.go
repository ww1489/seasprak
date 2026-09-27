package operations

import (
	"context"
	"slices"
	"sync"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

// Process is a single-execution channel-controlled fixture. It launches no OS
// process, changes no files and returns only inline content, never a log ref.
type Process struct {
	mu          sync.Mutex
	environment string
	release     <-chan struct{}
	output      string
	started     chan agent.ExecutionRef
	stop        chan struct{}
	done        chan struct{}
	ref         agent.ExecutionRef
	starts      int
	stopped     bool
}

func NewProcess(release <-chan struct{}, output string) *Process {
	return &Process{environment: "test-process:" + agent.MustID(), release: release, output: output, started: make(chan agent.ExecutionRef, 1), stop: make(chan struct{}), done: make(chan struct{})}
}
func (p *Process) Started() <-chan agent.ExecutionRef { return p.started }
func (p *Process) Starts() int                        { p.mu.Lock(); defer p.mu.Unlock(); return p.starts }
func (p *Process) ExecutionCapabilities(ctx context.Context) (agent.BackendCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return agent.BackendCapabilities{}, err
	}
	return capabilities(p.environment, "process"), nil
}
func (p *Process) Execute(ctx context.Context, r agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	none := agent.ProcessObservation{Terminated: true, SideEffect: "none"}
	f := r.Authorization.Frozen
	if !matchesBackend(f, p.environment, "process") || f.BackendID != "process-operations" || f.SandboxMode != "workspace-write" || !slices.Equal(r.Argv, f.Argv) || r.Shell != f.Shell || r.Cwd != f.Cwd || r.EnvironmentRef != f.EnvironmentRef || r.StdinRef != f.StdinRef || !slices.Equal(r.Mounts, f.Mounts) || r.TempRootRef != f.TempRootRef || r.OutputLimitBytes != f.OutputLimitBytes {
		return none, failure(product.CodePermissionDenied)
	}
	if err := r.Authorization.Validate(ctx); err != nil {
		return none, err
	}
	p.mu.Lock()
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return none, err
	}
	if p.starts != 0 {
		p.mu.Unlock()
		return none, failure(product.CodeStateConflict)
	}
	p.ref = agent.ExecutionRef{ID: agent.MustID(), BackendID: p.environment, CallID: f.CallID}
	p.starts++
	ref := p.ref
	p.mu.Unlock()
	defer close(p.done)
	p.started <- ref
	obs := agent.ProcessObservation{Execution: ref, Started: true, Terminated: true, Content: p.output, SideEffect: "none"}
	select {
	case <-ctx.Done():
		obs.Content = ""
		return obs, ctx.Err()
	case <-p.stop:
		obs.Content = ""
		return obs, context.Canceled
	case <-p.release:
		return obs, nil
	}
}
func (p *Process) Stop(ctx context.Context, ref agent.ExecutionRef) (agent.StopObservation, error) {
	if err := ctx.Err(); err != nil {
		return agent.StopObservation{}, err
	}
	p.mu.Lock()
	if p.starts == 0 || p.ref != ref {
		p.mu.Unlock()
		return agent.StopObservation{}, failure(product.CodeNotFound)
	}
	if !p.stopped {
		p.stopped = true
		close(p.stop)
	}
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return agent.StopObservation{}, ctx.Err()
	case <-p.done:
		return agent.StopObservation{Terminated: true, SideEffect: "none"}, nil
	}
}

var _ agent.ProcessOperations = (*Process)(nil)
var _ agent.BackendCapabilityReporter = (*Process)(nil)
