package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

type controlledProcessOperations struct {
	entered    chan struct{}
	release    chan struct{}
	starts     atomic.Int32
	validates  atomic.Int32
	err        error
	started    bool
	startedSet bool
	terminated bool
	sideEffect string
	capture    func(agent.AuthorizedProcess)
	last       agent.AuthorizedProcess
}

func (p *controlledProcessOperations) Execute(ctx context.Context, req agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	return p.executeValidated(ctx, req)
}

func (p *controlledProcessOperations) executeValidated(ctx context.Context, req agent.AuthorizedProcess) (agent.ProcessObservation, error) {
	p.validates.Add(1)
	if p.capture != nil {
		p.capture(req)
	}
	p.last = req
	if p.entered != nil {
		close(p.entered)
	}
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return agent.ProcessObservation{}, ctx.Err()
		}
	}
	p.starts.Add(1)
	started := true
	if p.startedSet {
		started = p.started
	}
	sideEffect := p.sideEffect
	if sideEffect == "" && p.err != nil {
		sideEffect = "unknown"
	}
	if p.err != nil {
		return agent.ProcessObservation{Started: started, Terminated: p.terminated, SideEffect: sideEffect}, p.err
	}
	if sideEffect == "" {
		sideEffect = "none"
	}
	return agent.ProcessObservation{Started: started, Terminated: true, ExitCode: 0, Content: "ok", SideEffect: sideEffect}, nil
}

func (*controlledProcessOperations) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}

func TestP2TicketCopiedAuthorizedRequestStartsOnlyOnce(t *testing.T) {
	proc := &controlledProcessOperations{}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Resources: []agent.ExecutionResource{{Identity: "file"}}, Argv: []string{"test"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	var captured agent.AuthorizedProcess
	proc.capture = func(req agent.AuthorizedProcess) { captured = req }
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	if err != nil || !out.Executed || proc.starts.Load() != 1 {
		t.Fatalf("err=%v out=%+v starts=%d", err, out, proc.starts.Load())
	}
	if _, err := proc.Execute(t.Context(), captured, nil); err == nil {
		t.Fatal("copied authorized request validated twice")
	}
	if proc.starts.Load() != 1 {
		t.Fatalf("replayed request increased starts to %d", proc.starts.Load())
	}
}

func TestP2TicketProcessRequestUsesFrozenPlanCopy(t *testing.T) {
	proc := &controlledProcessOperations{}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	argv := []string{"runner", "--value", "one"}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Resources: []agent.ExecutionResource{{Identity: "file"}}, Argv: argv, Cwd: "workspace/subdir", EnvironmentRef: "env:1", StdinRef: "stdin:1", TempRootRef: "temp:1", OutputLimitBytes: 123}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	argv[2] = "mutated-source"
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	if err != nil || !out.Executed {
		t.Fatalf("err=%v out=%+v", err, out)
	}
	got := proc.last
	if strings.Join(got.Argv, "|") != "runner|--value|one" || got.Cwd != "workspace/subdir" || got.EnvironmentRef != "env:1" || got.StdinRef != "stdin:1" || got.TempRootRef != "temp:1" || got.OutputLimitBytes != 123 {
		t.Fatalf("process request did not preserve frozen plan: %+v", got)
	}
	got.Argv[0] = "mutated-request"
	if proc.last.Authorization.Frozen.Argv[0] != "runner" {
		t.Fatal("authorized process argv aliases the frozen descriptor")
	}
}

func TestP2TicketProcessOperationsUsesClaimedTicketAndScheduler(t *testing.T) {
	proc := &controlledProcessOperations{}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	def := addDef(func(context.Context, json.RawMessage) (string, error) {
		t.Fatal("trusted Run bypassed controlled process operations")
		return "", nil
	})
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Resources: []agent.ExecutionResource{{Identity: "file"}}, Argv: []string{"test"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Content != "ok" || proc.validates.Load() != 1 || proc.starts.Load() != 1 || !hasIntent(sink) {
		t.Fatalf("out=%+v validates=%d starts=%d", out, proc.validates.Load(), proc.starts.Load())
	}
}

func TestP2TicketMissingBackendClaimsNothingAndStartsNothing(t *testing.T) {
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Argv: []string{"test"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeResourceUnavailable || out.Executed || hasIntent(sink) {
		t.Fatalf("err=%v out=%+v", err, out)
	}
}

func TestP2TicketFileAndArtifactOperationsAreReachable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend string
		args    string
		files   int32
		arts    int32
	}{
		{name: "file-write", backend: "file-operations", args: `{"operation":"write","path":"workspace/file","contentRef":"artifact:input"}`, files: 1},
		{name: "artifact-save", backend: "artifact-store", args: `{"operation":"save","contentRef":"output:1","mediaType":"text/plain","name":"log"}`, arts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := &controlledFileOperations{}
			artifacts := &controlledArtifactStore{}
			sink := &recordSink{found: true, rec: accepted(tc.args)}
			def := Definition{Version: "1", Name: "add", Schema: json.RawMessage(`{"type":"object"}`)}
			def.Execution = ExecutionDescription{Effect: "write", BackendID: tc.backend, Resources: []agent.ExecutionResource{{Identity: "file"}}}
			exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
				WithOperations(Operations{Files: files, Artifacts: artifacts}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", tc.args)
			if err != nil || !out.Executed || files.writes.Load() != tc.files || artifacts.saves.Load() != tc.arts {
				t.Fatalf("err=%v out=%+v files=%d artifacts=%d", err, out, files.writes.Load(), artifacts.saves.Load())
			}
		})
	}
}

type controlledFileOperations struct {
	writes atomic.Int32
	effect agent.FileEffect
	err    error
}

func (*controlledFileOperations) List(context.Context, agent.ListRequest) (agent.ListResult, error) {
	return agent.ListResult{}, nil
}
func (*controlledFileOperations) Read(context.Context, agent.ReadRequest) (agent.ReadResult, error) {
	return agent.ReadResult{}, nil
}
func (f *controlledFileOperations) Write(ctx context.Context, req agent.AuthorizedFileWrite) (agent.FileEffect, error) {
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.FileEffect{}, err
	}
	f.writes.Add(1)
	if f.effect.Identity == "" && f.effect.SideEffect == "" && !f.effect.Confirmed && f.err == nil {
		return agent.FileEffect{Identity: req.Path, Confirmed: true, SideEffect: "confirmed"}, nil
	}
	effect := f.effect
	if effect.Identity == "" {
		effect.Identity = req.Path
	}
	return effect, f.err
}
func (*controlledFileOperations) Edit(context.Context, agent.AuthorizedFileEdit) (agent.FileEffect, error) {
	return agent.FileEffect{}, nil
}
func (*controlledFileOperations) Search(context.Context, agent.SearchRequest) (agent.SearchResult, error) {
	return agent.SearchResult{}, nil
}

type controlledArtifactStore struct{ saves atomic.Int32 }

func (a *controlledArtifactStore) Save(ctx context.Context, input agent.ArtifactInput) (agent.ArtifactRef, error) {
	if err := input.Authorization.Validate(ctx); err != nil {
		return agent.ArtifactRef{}, err
	}
	a.saves.Add(1)
	return agent.ArtifactRef{ID: "artifact:1", Available: true}, nil
}
func (*controlledArtifactStore) Open(context.Context, agent.ArtifactRead) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

type testTicketValidator func(context.Context, agent.FrozenExecution) error

func (f testTicketValidator) ValidateExecutionTicket(ctx context.Context, frozen agent.FrozenExecution) error {
	return f(ctx, frozen)
}

type blockingExecutionTicketValidator struct {
	entered chan struct{}
	release chan struct{}
}

func (v *blockingExecutionTicketValidator) ValidateExecutionTicket(context.Context, agent.FrozenExecution) error {
	close(v.entered)
	<-v.release
	return nil
}

func TestP2TicketCancelDuringValidatorNeverStartsBackend(t *testing.T) {
	proc := &controlledProcessOperations{}
	validator := &blockingExecutionTicketValidator{entered: make(chan struct{}), release: make(chan struct{})}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`), validator: validator}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Argv: []string{"test"}, Timeout: time.Minute}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		out, err := exec.Run(ctx, agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
		result <- err
		if out.Executed || out.Status != "failed" {
			t.Errorf("out=%+v", out)
		}
	}()
	<-validator.entered
	cancel()
	close(validator.release)
	if err := <-result; err != nil {
		t.Fatalf("run returned infrastructure error=%v", err)
	}
	if proc.validates.Load() != 0 || proc.starts.Load() != 0 {
		t.Fatalf("backend calls=%d starts=%d", proc.validates.Load(), proc.starts.Load())
	}
}

func TestP2TicketExpiryDuringValidatorNeverStartsBackend(t *testing.T) {
	proc := &controlledProcessOperations{}
	validator := &blockingExecutionTicketValidator{entered: make(chan struct{}), release: make(chan struct{})}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`), validator: validator}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Argv: []string{"test"}, Timeout: 20 * time.Millisecond}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
		if out.Executed || out.Status != "failed" {
			t.Errorf("out=%+v", out)
		}
		result <- err
	}()
	<-validator.entered
	time.Sleep(30 * time.Millisecond)
	close(validator.release)
	if err := <-result; err != nil {
		t.Fatalf("run returned infrastructure error=%v", err)
	}
	if proc.validates.Load() != 0 || proc.starts.Load() != 0 {
		t.Fatalf("backend calls=%d starts=%d", proc.validates.Load(), proc.starts.Load())
	}
}

type deadlineCaptureProcess struct {
	deadline time.Time
}

func (p *deadlineCaptureProcess) Execute(ctx context.Context, req agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return agent.ProcessObservation{}, errors.New("execution context has no deadline")
	}
	p.deadline = deadline
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	return agent.ProcessObservation{Started: false, Terminated: true, SideEffect: "none"}, nil
}

func (*deadlineCaptureProcess) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}

func TestP2TicketAndRunContextShareParentDeadline(t *testing.T) {
	proc := &deadlineCaptureProcess{}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Argv: []string{"test"}, Timeout: time.Minute}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	parentDeadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(t.Context(), parentDeadline)
	defer cancel()
	out, err := exec.Run(ctx, agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	if err != nil || out.Executed || out.SideEffect != "none" {
		t.Fatalf("err=%v out=%+v", err, out)
	}
	if !proc.deadline.Equal(parentDeadline) {
		t.Fatalf("run deadline=%v want=%v", proc.deadline, parentDeadline)
	}
}

func TestP2TicketRevokedBeforeOperationsCallIsZeroBackendCalls(t *testing.T) {
	proc := &controlledProcessOperations{}
	var checks atomic.Int32
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	sink.validator = testTicketValidator(func(context.Context, agent.FrozenExecution) error {
		checks.Add(1)
		return product.NewError(product.CodePermissionDenied, "policy revoked")
	})
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Argv: []string{"test"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	if err != nil || checks.Load() != 1 || proc.validates.Load() != 0 || proc.starts.Load() != 0 || !hasIntent(sink) || out.Executed || !strings.Contains(out.Content, product.CodePermissionDenied) {
		t.Fatalf("err=%v out=%+v checks=%d calls=%d starts=%d", err, out, checks.Load(), proc.validates.Load(), proc.starts.Load())
	}
}

func TestP2TicketBackendFailureCountsOneStart(t *testing.T) {
	backendErr := errors.New("backend failed")
	proc := &controlledProcessOperations{err: backendErr, terminated: true}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Argv: []string{"test"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	if !out.Executed || out.SideEffect != "unknown" || proc.validates.Load() != 1 || proc.starts.Load() != 1 {
		t.Fatalf("err=%v out=%+v validates=%d starts=%d", err, out, proc.validates.Load(), proc.starts.Load())
	}
}

func TestP2TicketUnknownBackendRetainsConflictLease(t *testing.T) {
	scheduler := NewResourceScheduler()
	unknownBackend := &controlledProcessOperations{err: context.DeadlineExceeded}
	firstSink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Resources: []agent.ExecutionResource{{Identity: "file"}}, Argv: []string{"test"}}
	first, err := NewExecutor("gen", []Definition{def}, firstSink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: unknownBackend}), WithResourceScheduler(scheduler), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := first.Run(t.Context(), agent.ExecutionScope{SessionID: "session-one"}, "prov-1", "add", `{"n":1}`)
	if err != nil || !out.Executed || out.SideEffect != "unknown" || unknownBackend.starts.Load() != 1 {
		t.Fatalf("err=%v out=%+v starts=%d", err, out, unknownBackend.starts.Load())
	}

	secondBackend := &controlledProcessOperations{}
	secondSink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	second, err := NewExecutor("gen", []Definition{def}, secondSink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: secondBackend}), WithResourceScheduler(scheduler), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := second.Run(ctx, agent.ExecutionScope{SessionID: "session-two"}, "prov-1", "add", `{"n":1}`)
		result <- err
	}()
	waitResourceQueue(t, scheduler, 1)
	if secondBackend.starts.Load() != 0 || hasIntent(secondSink) {
		t.Fatal("conflicting work started while prior outcome was unknown")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("wait cancellation=%v", err)
	}
	if secondBackend.starts.Load() != 0 || hasIntent(secondSink) {
		t.Fatal("cancelled conflicting waiter started or claimed")
	}
}

func TestP2ResourcesTerminatedUnknownProcessRetainsHold(t *testing.T) {
	scheduler := NewResourceScheduler()
	proc := &controlledProcessOperations{terminated: true, sideEffect: "unknown"}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Resources: []agent.ExecutionResource{{Identity: "file"}}, Argv: []string{"test"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(scheduler), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	if err != nil || !out.Executed || out.Status != "succeeded" || out.SideEffect != "unknown" {
		t.Fatalf("err=%v out=%+v", err, out)
	}
	if !scheduler.HasHold(ResourceHoldID("session", "prod-1")) {
		t.Fatal("terminated process with unknown side effect released its hold")
	}
}

func TestP2ResourcesUnknownFileEffectRetainsHoldAndObservation(t *testing.T) {
	scheduler := NewResourceScheduler()
	files := &controlledFileOperations{effect: agent.FileEffect{SideEffect: "unknown"}}
	sink := &recordSink{found: true, rec: accepted(`{"operation":"write","path":"file","contentRef":"content:1"}`)}
	def := Definition{Version: "1", Name: "add", Schema: json.RawMessage(`{"type":"object"}`), Execution: ExecutionDescription{Effect: "write", BackendID: "file-operations", Resources: []agent.ExecutionResource{{Identity: "file"}}}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Files: files}), WithResourceScheduler(scheduler), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"operation":"write","path":"file","contentRef":"content:1"}`)
	if err != nil || out.Executed || out.Status != "outcome_unknown" || out.SideEffect != "unknown" {
		t.Fatalf("err=%v out=%+v", err, out)
	}
	if !scheduler.HasHold(ResourceHoldID("session", "prod-1")) {
		t.Fatal("unconfirmed file effect released its hold")
	}
	observation := lastObservation(t, sink)
	if observation.Status != "outcome_unknown" || observation.SideEffect != "unknown" || observation.Executed {
		t.Fatalf("observation=%+v", observation)
	}
}

func TestP2ResourcesConfirmedTerminalResultsReleaseHold(t *testing.T) {
	for _, tc := range []struct {
		name string
		proc *controlledProcessOperations
	}{
		{name: "success-none", proc: &controlledProcessOperations{}},
		{name: "failure-confirmed", proc: &controlledProcessOperations{err: errors.New("process failed"), terminated: true, sideEffect: "confirmed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheduler := NewResourceScheduler()
			sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
			def := addDef(nil)
			def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Resources: []agent.ExecutionResource{{Identity: "file"}}, Argv: []string{"test"}}
			exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
				WithOperations(Operations{Process: tc.proc}), WithResourceScheduler(scheduler), WithResourceDomain("memory", "workspace"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
			if err != nil || !out.Executed || out.SideEffect == "unknown" {
				t.Fatalf("err=%v out=%+v", err, out)
			}
			if scheduler.HasHold(ResourceHoldID("session", "prod-1")) {
				t.Fatal("confirmed terminal result retained a hold")
			}
		})
	}
}

func lastObservation(t *testing.T, sink *recordSink) agent.ToolObservation {
	t.Helper()
	for i := len(sink.facts) - 1; i >= 0; i-- {
		if sink.facts[i].Kind != "tool_observation" {
			continue
		}
		var record agent.ToolRecord
		if err := json.Unmarshal(sink.facts[i].Payload, &record); err != nil {
			t.Fatal(err)
		}
		if record.Observation != nil {
			return *record.Observation
		}
	}
	t.Fatal("tool observation not found")
	return agent.ToolObservation{}
}

func TestP2ResourcesKnownTerminalFailureRetainsHold(t *testing.T) {
	scheduler := NewResourceScheduler()
	proc := &controlledProcessOperations{err: errors.New("process failed"), terminated: true}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Resources: []agent.ExecutionResource{{Identity: "file"}}, Argv: []string{"test"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(scheduler), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	if err != nil || !out.Executed || proc.starts.Load() != 1 {
		t.Fatalf("err=%v out=%+v starts=%d", err, out, proc.starts.Load())
	}
	if !scheduler.HasHold(ResourceHoldID("session", "prod-1")) {
		t.Fatal("terminal failure with unknown side effect released its conflict hold")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = scheduler.Acquire(ctx, ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "file"}}, Effect: "write"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unknown hold allowed conflicting acquire: %v", err)
	}
}

func TestP2ResourcesObservationFailureRetainsStartedCallHold(t *testing.T) {
	scheduler := NewResourceScheduler()
	saveErr := errors.New("observation append failed")
	proc := &controlledProcessOperations{}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`), beforeCommit: func(f agent.Fact) error {
		if f.Kind == "tool_observation" {
			return saveErr
		}
		return nil
	}}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Resources: []agent.ExecutionResource{{Identity: "file"}}, Argv: []string{"test"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(scheduler), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	if !errors.Is(err, saveErr) || !out.Executed || proc.starts.Load() != 1 {
		t.Fatalf("err=%v out=%+v starts=%d", err, out, proc.starts.Load())
	}
	id := ResourceHoldID("session", "prod-1")
	scheduler.mu.Lock()
	_, retained := scheduler.holds[id]
	scheduler.mu.Unlock()
	if !retained {
		t.Fatal("started call lost its conflict hold after observation failure")
	}
	ctx, cancel := context.WithCancel(t.Context())
	blocked := make(chan error, 1)
	go func() {
		_, err := scheduler.Acquire(ctx, ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "file"}}, Effect: "write"})
		blocked <- err
	}()
	waitResourceQueue(t, scheduler, 1)
	cancel()
	if err := <-blocked; !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked err=%v", err)
	}
}

func TestP2ResourcesNoStartUnknownRetainsHoldAfterSavedObservation(t *testing.T) {
	assertNoStartHold(t, false)
}

func TestP2ResourcesNoStartUnknownRetainsHoldAfterObservationFailure(t *testing.T) {
	assertNoStartHold(t, true)
}

func assertNoStartHold(t *testing.T, failObservation bool) {
	t.Helper()
	scheduler := NewResourceScheduler()
	proc := &controlledProcessOperations{startedSet: true, started: false, terminated: true, sideEffect: "unknown"}
	saveErr := errors.New("observation append failed")
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	if failObservation {
		sink.beforeCommit = func(f agent.Fact) error {
			if f.Kind == "tool_observation" {
				return saveErr
			}
			return nil
		}
	}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Resources: []agent.ExecutionResource{{Identity: "file"}}, Argv: []string{"test"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(scheduler), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	if failObservation {
		if !errors.Is(err, saveErr) {
			t.Fatalf("observation failure=%v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if out.Status != "outcome_unknown" || out.SideEffect != "unknown" || out.Executed {
		t.Fatalf("out=%+v", out)
	}
	if !scheduler.HasHold(ResourceHoldID("session", "prod-1")) {
		t.Fatal("no-start unknown result released its hold")
	}
	ctx, cancel := context.WithCancel(t.Context())
	blocked := make(chan error, 1)
	go func() {
		_, err := scheduler.Acquire(ctx, ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "file"}}, Effect: "write"})
		blocked <- err
	}()
	waitResourceQueue(t, scheduler, 1)
	cancel()
	if err := <-blocked; !errors.Is(err, context.Canceled) {
		t.Fatalf("conflicting waiter=%v", err)
	}
}

func TestP2ResourcesNoStartNoneReleasesLease(t *testing.T) {
	scheduler := NewResourceScheduler()
	proc := &controlledProcessOperations{startedSet: true, started: false, terminated: true, sideEffect: "none"}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Resources: []agent.ExecutionResource{{Identity: "file"}}, Argv: []string{"test"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(scheduler), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	if err != nil || out.Executed || out.SideEffect != "none" {
		t.Fatalf("err=%v out=%+v", err, out)
	}
	if scheduler.HasHold(ResourceHoldID("session", "prod-1")) {
		t.Fatal("trusted no-start none retained a hold")
	}
	lease, err := scheduler.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "file"}}, Effect: "write"})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
}

func TestP2PolicyControlledBackendErrorDoesNotExposePrivateDetails(t *testing.T) {
	private := "synthetic-private-backend-value"
	proc := &controlledProcessOperations{err: errors.New(private), terminated: true}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Argv: []string{"test"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "private-error"}, "prov-1", "add", `{"n":1}`)
	obs := lastObservation(t, sink)
	if err != nil || proc.starts.Load() != 1 || !out.Executed || strings.Contains(out.Content, private) || strings.Contains(obs.Content, private) {
		t.Fatalf("err=%v out=%+v observation=%+v", err, out, obs)
	}
	trusted := addDef(func(context.Context, json.RawMessage) (string, error) { return "", errors.New(private) })
	trustedSink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	trustedExec, err := NewExecutor("gen", []Definition{trusted}, trustedSink, allow{}, agent.NewBudget(config.DefaultLimits()), WithResourceScheduler(NewResourceScheduler()))
	if err != nil {
		t.Fatal(err)
	}
	trustedOut, err := trustedExec.Run(t.Context(), agent.ExecutionScope{SessionID: "private-trusted"}, "prov-1", "add", `{"n":1}`)
	if err != nil || !trustedOut.Executed || strings.Contains(trustedOut.Content, private) || strings.Contains(lastObservation(t, trustedSink).Content, private) {
		t.Fatalf("trusted err=%v out=%+v", err, trustedOut)
	}
}

func TestP2TicketClaimFailureNeverCallsBackend(t *testing.T) {
	proc := &controlledProcessOperations{}
	disk := errors.New("claim failed")
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`), beforeCommit: func(f agent.Fact) error {
		if f.Kind == "tool_intent" {
			return disk
		}
		return nil
	}}
	def := addDef(nil)
	def.Execution = ExecutionDescription{Effect: "write", BackendID: "process-operations", Argv: []string{"test"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: proc}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	if !errors.Is(err, disk) || proc.validates.Load() != 0 || proc.starts.Load() != 0 {
		t.Fatalf("err=%v validates=%d starts=%d", err, proc.validates.Load(), proc.starts.Load())
	}
}
