package sessions

import (
	"context"
	"errors"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

// This backend admits the request but starts no process. The first response
// deliberately cannot establish that fact; only its separate read-only query
// supplies trusted no-start evidence. No operating-system process is launched.
type diskUnknownProcess struct {
	calls atomic.Int32
}

func (p *diskUnknownProcess) Execute(ctx context.Context, req agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.calls.Add(1)
	return agent.ProcessObservation{SideEffect: "unknown", Terminated: true}, errors.New("controlled admission response unavailable")
}
func (*diskUnknownProcess) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}

type diskReleasedProcess struct {
	calls   atomic.Int32
	entered chan struct{}
	check   func()
}

func (p *diskReleasedProcess) Execute(ctx context.Context, req agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.calls.Add(1)
	p.check()
	close(p.entered)
	return agent.ProcessObservation{Started: true, Terminated: true, SideEffect: "none", Content: "done"}, nil
}
func (*diskReleasedProcess) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}

func TestP2ResourcesDiskReopenUnknownBlocksUntilDurableReconcile(t *testing.T) {
	ctx := t.Context()
	queryEntered, queryRelease := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-queryRelease:
		default:
			close(queryRelease)
		}
	}()
	var queries atomic.Int32
	original := &diskUnknownProcess{}
	model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "execute", Name: "execute", Arguments: `{"argv":["echo","hi"],"cwd":"workspace"}`}}}, testkit.Step{Text: "done"})
	opts := Options{
		SessionID: "disk-unknown", Workspace: t.TempDir(), StateRoot: t.TempDir(),
		Profile: ProfileMemory, Principal: "operator", Model: model,
		Tools:      []tools.Definition{builtinDefinitionForSession(t, "execute")},
		Operations: tools.Operations{Process: original}, ResourceScheduler: tools.NewResourceScheduler(),
		ReconcileQueries: map[string]ReconcileQuery{"no-start": ReconcileQueryFunc(func(ctx context.Context, _ ReconcileQueryRequest) (ReconcileEvidence, error) {
			queries.Add(1)
			close(queryEntered)
			select {
			case <-queryRelease:
			case <-ctx.Done():
				return ReconcileEvidence{}, ctx.Err()
			}
			return ReconcileEvidence{TrustedNoStart: true, EvidenceSource: "controlled-admission-ledger", EvidenceRefs: []string{"admission-not-started"}}, nil
		})},
	}
	first, err := CreateAgentSession(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close(context.Background()) })
	if _, ok := first.rt.opts.Store.(*jsonl.Store); !ok {
		t.Fatal("fixture is not backed by a real JSONL store")
	}
	receipt := submitOutput(t, first)
	waitResumeCondition(t, func() bool {
		view := first.rt.manager.View()
		return view.Traces[receipt.TraceID].ExecutionStopped && terminal(view.Traces[receipt.TraceID].State)
	})
	before := first.rt.manager.View()
	call := onlyControlledCall(t, first)
	originalModelCalls := model.Calls()
	if !call.Claimed || call.Observation == nil || call.Observation.SideEffect != "unknown" || !before.Traces[call.Scope.TraceID].ExecutionStopped || original.calls.Load() != 1 {
		t.Fatalf("unknown command did not durably stop: call=%+v runs=%d", call, original.calls.Load())
	}
	holdID := tools.ResourceHoldID(opts.SessionID, call.Call.CallID)
	if !opts.ResourceScheduler.HasHold(holdID) {
		t.Fatal("unknown command lost its original hold")
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Discard the entire process-local scheduling state, then use the public
	// disk-open path; reusing first.rt.manager would not prove reconstruction.
	opts.ResourceScheduler = tools.NewResourceScheduler()
	opened, err := OpenAgentSession(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	if _, ok := opened.rt.opts.Store.(*jsonl.Store); !ok {
		t.Fatal("reopen did not use JSONL")
	}
	if opened.rt.opts.Store == first.rt.opts.Store || !opts.ResourceScheduler.HasHold(holdID) || original.calls.Load() != 1 || model.Calls() != originalModelCalls {
		t.Fatal("public Open failed to reconstruct the hold without executing work")
	}

	command := func(query, evidence string) ReconcileCommand {
		view := opened.rt.manager.View()
		latest := latestReconcileObservation(view, call.Call.CallID)
		return ReconcileCommand{TraceID: call.Scope.TraceID, InvocationID: call.Scope.InvocationID, CallID: call.Call.CallID,
			ObservationID: latest.ID, ObservationVersion: latest.Version, ExpectedRevision: view.LastSeq, QueryID: query, EvidenceRef: evidence}
	}
	if _, err := opened.Reconcile(ctx, command("", "manual-no-start-assertion")); err != nil {
		t.Fatal(err)
	}
	if !opts.ResourceScheduler.HasHold(holdID) || !opened.rt.manager.View().HasUnresolvedEffects() || len(opened.rt.manager.View().ResourceHoldReleases) != 0 {
		t.Fatal("untrusted evidence released a durable unknown hold")
	}

	secondProcess := &diskReleasedProcess{entered: make(chan struct{})}
	secondProcess.check = func() {
		// This assertion runs inside the actual backend, not after completion:
		// execution is allowed only after the release is present on disk.
		loaded, err := opened.rt.opts.Store.Load(context.Background(), opts.SessionID)
		if err != nil {
			t.Error(err)
			return
		}
		found := false
		for _, commit := range loaded.Commits {
			types := map[string]int{}
			for _, record := range commit.ControlRecords {
				types[record.Type]++
			}
			if types["resource_hold_release"] == 1 {
				found = true
				if types["reconciliation"] != 1 || types["observation_revision"] != 1 || types["operation"] != 1 {
					t.Error("release was not committed atomically with reconciliation")
				}
			}
		}
		if !found {
			t.Error("conflicting backend started before durable release")
		}
	}
	secondModel := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "execute", Name: "execute", Arguments: `{"argv":["echo","hi"],"cwd":"workspace"}`}}}, testkit.Step{Text: "done"})
	secondOpts := Options{SessionID: "disk-conflicting", Workspace: opts.Workspace, StateRoot: opts.StateRoot, Profile: ProfileMemory,
		Model: secondModel, Tools: opts.Tools, Operations: tools.Operations{Process: secondProcess}, ResourceScheduler: opts.ResourceScheduler}
	second, err := CreateAgentSession(ctx, secondOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	secondReceipt := submitOutput(t, second)
	// No production hook exposes scheduler queue admission. Observe the real
	// blocked Acquire stack (not a pre-authorization hook), then hold the
	// reconciliation query at a channel boundary while checking zero starts.
	waitDiskResourceAcquire(t, opts.ResourceScheduler)
	result := make(chan error, 1)
	cmd := command("no-start", "")
	go func() { _, err := opened.Reconcile(ctx, cmd); result <- err }()
	select {
	case <-queryEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("trusted query did not enter")
	}
	if secondProcess.calls.Load() != 0 || original.calls.Load() != 1 || !opts.ResourceScheduler.HasHold(holdID) {
		t.Fatalf("conflicting backend ran before release: original=%d second=%d", original.calls.Load(), secondProcess.calls.Load())
	}
	view := second.rt.manager.View()
	if onlyControlledCall(t, second).Claimed || len(view.Traces) != 1 {
		t.Fatal("waiting conflict already claimed execution")
	}
	close(queryRelease)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconciliation did not finish")
	}
	select {
	case <-secondProcess.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("durable release did not unblock the conflicting session")
	}
	waitResumeCondition(t, func() bool { return terminal(second.rt.manager.View().Traces[secondReceipt.TraceID].State) })
	after := opened.rt.manager.View()
	if original.calls.Load() != 1 || secondProcess.calls.Load() != 1 || queries.Load() != 1 || model.Calls() != originalModelCalls || secondModel.Calls() != 2 || opts.ResourceScheduler.HasHold(holdID) || after.HasUnresolvedEffects() {
		t.Fatalf("unexpected executions/holds: original=%d second=%d queries=%d", original.calls.Load(), secondProcess.calls.Load(), queries.Load())
	}
	if after.Calls[call.Call.CallID].Observation.SideEffect != "unknown" || after.Traces[call.Scope.TraceID].State != before.Traces[call.Scope.TraceID].State || after.Traces[call.Scope.TraceID].Usage != before.Traces[call.Scope.TraceID].Usage {
		t.Fatalf("reconcile changed original history: effect=%s state=%s/%s usage=%+v/%+v", after.Calls[call.Call.CallID].Observation.SideEffect, before.Traces[call.Scope.TraceID].State, after.Traces[call.Scope.TraceID].State, before.Traces[call.Scope.TraceID].Usage, after.Traces[call.Scope.TraceID].Usage)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(ctx); err != nil {
		t.Fatal(err)
	}
	opts.ResourceScheduler = tools.NewResourceScheduler()
	reopened, err := OpenAgentSession(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	if opts.ResourceScheduler.HasHold(holdID) || reopened.rt.manager.View().HasUnresolvedEffects() || len(reopened.rt.manager.View().ResourceHoldReleases) != 1 || original.calls.Load() != 1 {
		t.Fatal("second disk reopen lost durable release or reran original work")
	}
	t.Logf("JSONL close/open twice; original backend=%d conflicting backend=%d trusted query=%d models=%d/%d", original.calls.Load(), secondProcess.calls.Load(), queries.Load(), model.Calls(), secondModel.Calls())
}

func waitDiskResourceAcquire(t *testing.T, scheduler *tools.ResourceScheduler) {
	t.Helper()
	acquire := fmt.Sprintf("tools.(*ResourceScheduler).Acquire(%p", scheduler)
	waitResumeCondition(t, func() bool {
		buf := make([]byte, 1<<20)
		n := goruntime.Stack(buf, true)
		for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(stack, "[select]") && strings.Contains(stack, acquire) && strings.Contains(stack, "tools.(*Executor).run(") {
				return true
			}
		}
		return false
	})
}

func (*diskUnknownProcess) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return sessionFakeCapabilities("disk-unknown-process"), nil
}
func (*diskReleasedProcess) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return sessionFakeCapabilities("disk-released-process"), nil
}

var _ agent.ProcessOperations = (*diskUnknownProcess)(nil)
var _ agent.ProcessOperations = (*diskReleasedProcess)(nil)
