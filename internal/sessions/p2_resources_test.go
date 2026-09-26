package sessions

import (
	"context"
	"encoding/json"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type sessionControlledProcess struct {
	mu       sync.Mutex
	active   int
	max      int
	entered  chan string
	releases map[string]chan struct{}
	starts   atomic.Int32
	calls    atomic.Int32
}

func (p *sessionControlledProcess) Execute(ctx context.Context, request agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	p.calls.Add(1)
	if err := request.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.starts.Add(1)
	id := request.Authorization.Frozen.Scope.SessionID
	p.mu.Lock()
	p.active++
	if p.active > p.max {
		p.max = p.active
	}
	p.mu.Unlock()
	if p.entered != nil {
		p.entered <- id
	}
	if release := p.releases[id]; release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return agent.ProcessObservation{Started: true, SideEffect: "unknown"}, ctx.Err()
		}
	}
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return agent.ProcessObservation{Started: true, Terminated: true, Content: "ok", SideEffect: "none"}, nil
}
func (*sessionControlledProcess) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}

func TestP2StartRejectsNilManagerBeforeViewOrPolicy(t *testing.T) {
	backend, err := memory.Open("nil-manager", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	_, err = Start(Options{SessionID: "nil-manager", Workspace: "workspace", Profile: ProfileMemory, Store: backend, Model: testkit.NewFake(), ResourceScheduler: tools.NewResourceScheduler()}, nil, "gen")
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
		t.Fatalf("nil manager error=%v", err)
	}
}

func TestP2ResourcesStartRestoresMissingFrozenAsWorkspaceGuard(t *testing.T) {
	backend, err := memory.Open("missing-frozen", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	manager, err := state.NewManager(backend, "missing-frozen")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	receipt, err := manager.Accept(ctx, agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run"}`)}, agent.TargetAgent{Name: "main", Version: "main-v1", Generation: "gen"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetTraceState(ctx, receipt.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	trace := manager.View().Traces[receipt.TraceID]
	scope := agent.ExecutionScope{SessionID: "missing-frozen", TraceID: receipt.TraceID, InvocationID: trace.InvocationID, TurnID: "turn", ExecutionID: "execution", Generation: "gen"}
	if err := manager.SaveTurn(ctx, agent.TurnRecord{ID: scope.TurnID, TraceID: scope.TraceID, InvocationID: scope.InvocationID}); err != nil {
		t.Fatal(err)
	}
	call := agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: "call", ProviderCallID: "provider", Name: "work", Arguments: `{}`, Generation: "gen", Hash: "call-hash"}}
	msg := agent.AgentMessage{ID: "assistant", Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: scope.SessionID, TraceID: scope.TraceID, TurnID: scope.TurnID, InvocationID: scope.InvocationID}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider", Name: "work", Arguments: `{}`})}}}
	if err := manager.SaveAssistant(ctx, msg, []agent.ToolRecord{call}); err != nil {
		t.Fatal(err)
	}
	usage := trace.Usage
	usage.ToolExecutions++
	if err := manager.ClaimTool(ctx, call.Call, usage); err != nil {
		t.Fatal(err)
	}

	scheduler := tools.NewResourceScheduler()
	opts := Options{SessionID: "missing-frozen", Workspace: "workspace", Profile: ProfileMemory, Store: backend, Model: testkit.NewFake(), ResourceScheduler: scheduler, ResourceEnvironment: "memory"}
	first, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close(context.Background())
	second, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(context.Background())
	holdID := tools.ResourceHoldID("missing-frozen", "call")
	if !scheduler.HasHold(holdID) {
		t.Fatal("missing frozen execution did not restore a conservative workspace hold")
	}

	waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err = scheduler.Acquire(waitCtx, tools.ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "other"}}, Effect: "read"})
	if err != context.DeadlineExceeded {
		t.Fatalf("workspace guard did not block declared work: %v", err)
	}

	known := manager.View().Calls["call"]
	known.Observation = &agent.ToolObservation{Status: "succeeded", SideEffect: "none", Executed: true}
	if err := manager.SaveCall(ctx, known); err != nil {
		t.Fatal(err)
	}
	third, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close(context.Background())
	if scheduler.HasHold(holdID) {
		t.Fatal("known terminal result did not release conservative workspace hold")
	}
}

func TestP2ResourcesStartRestoresUnknownHoldIdempotentlyAndKnownResultReleases(t *testing.T) {
	backend, err := memory.Open("restore-hold", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	manager, err := state.NewManager(backend, "restore-hold")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	receipt, err := manager.Accept(ctx, agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run"}`)}, agent.TargetAgent{Name: "main", Version: "main-v1", Generation: "gen"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetTraceState(ctx, receipt.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	trace := manager.View().Traces[receipt.TraceID]
	scope := agent.ExecutionScope{SessionID: "restore-hold", TraceID: receipt.TraceID, InvocationID: trace.InvocationID, TurnID: "turn", ExecutionID: "execution", Generation: "gen"}
	if err := manager.SaveTurn(ctx, agent.TurnRecord{ID: scope.TurnID, TraceID: scope.TraceID, InvocationID: scope.InvocationID}); err != nil {
		t.Fatal(err)
	}
	call := agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: "call", ProviderCallID: "provider", Name: "work", Arguments: `{}`, Generation: "gen", Hash: "call-hash"}}
	msg := agent.AgentMessage{ID: "assistant", Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: scope.SessionID, TraceID: scope.TraceID, TurnID: scope.TurnID, InvocationID: scope.InvocationID}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider", Name: "work", Arguments: `{}`})}}}
	if err := manager.SaveAssistant(ctx, msg, []agent.ToolRecord{call}); err != nil {
		t.Fatal(err)
	}
	frozen := agent.FrozenExecution{ID: "execution:call", CallID: "call", Scope: scope, Origin: "model", Tool: "work", ToolVersion: "1", Generation: "gen", ProviderCallID: "provider", OriginalArgumentsHash: toolArgumentHash([]byte(`{}`)), FinalArgumentsHash: toolArgumentHash([]byte(`{}`)), ArgumentsRef: "arguments:call", FinalArguments: json.RawMessage(`{}`), Resources: []agent.ExecutionResource{{Identity: "file"}}, Effect: "write", Concurrency: "exclusive", BackendID: "process-operations", Argv: []string{"test"}, PolicyRef: "policy"}
	frozen.Hash, err = frozen.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveRecords(ctx, manager.View().LastSeq, state.Records{FrozenExecutions: []state.FrozenExecution{frozen}}); err != nil {
		t.Fatal(err)
	}
	usage := trace.Usage
	usage.ToolExecutions++
	if err := manager.ClaimTool(ctx, call.Call, usage); err != nil {
		t.Fatal(err)
	}
	scheduler := tools.NewResourceScheduler()
	opts := Options{SessionID: "restore-hold", Workspace: "workspace", Profile: ProfileMemory, Store: backend, Model: testkit.NewFake(), ResourceScheduler: scheduler, ResourceEnvironment: "memory"}
	first, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close(context.Background())
	second, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(context.Background())
	holdID := tools.ResourceHoldID("restore-hold", "call")
	if !scheduler.HasHold(holdID) {
		t.Fatal("reopen did not restore the unresolved hold")
	}
	claimed := manager.View().Calls["call"]
	claimed.Observation = &agent.ToolObservation{Status: "succeeded", SideEffect: "none", Executed: true}
	if err := manager.SaveCall(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	third, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close(context.Background())
	lease, err := scheduler.Acquire(ctx, tools.ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "file"}}, Effect: "write"})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
}

func TestP2ResourcesSameWorkspaceSessionsDoNotOverlapWrites(t *testing.T) {
	workspace := t.TempDir()
	entered := make(chan string, 2)
	releaseOne := make(chan struct{})
	releaseTwo := make(chan struct{})
	backend := &sessionControlledProcess{entered: entered, releases: map[string]chan struct{}{"resource-one": releaseOne, "resource-two": releaseTwo}}
	definition := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "process-operations", Effect: "write", Resources: []agent.ExecutionResource{{Identity: "shared-file"}}, Argv: []string{"test"}}}
	start := func(id string) (*AgentSession, agent.InputReceipt) {
		model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "done"})
		s, err := CreateAgentSession(t.Context(), Options{Workspace: workspace, StateRoot: "memory", SessionID: id, Profile: ProfileMemory, Model: model, Tools: []tools.Definition{definition}, Operations: tools.Operations{Process: backend}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close(context.Background()) })
		receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"work"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return s, receipt
	}
	one, first := start("resource-one")
	two, second := start("resource-two")
	if got := <-entered; got != "resource-one" && got != "resource-two" {
		t.Fatalf("unexpected session %q", got)
	} else if got == "resource-one" {
		select {
		case other := <-entered:
			t.Fatalf("session %s overlapped first writer", other)
		default:
		}
		close(releaseOne)
		if next := <-entered; next != "resource-two" {
			t.Fatalf("next=%s", next)
		}
		close(releaseTwo)
	} else {
		select {
		case other := <-entered:
			t.Fatalf("session %s overlapped first writer", other)
		default:
		}
		close(releaseTwo)
		if next := <-entered; next != "resource-one" {
			t.Fatalf("next=%s", next)
		}
		close(releaseOne)
	}
	if backend.starts.Load() != 2 || backend.max != 1 {
		t.Fatalf("starts=%d max=%d", backend.starts.Load(), backend.max)
	}
	waitSessionTrace(t, one, first.TraceID, "completed")
	waitSessionTrace(t, two, second.TraceID, "completed")
}

func waitSessionTrace(t *testing.T, session *AgentSession, traceID, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if trace := session.rt.manager.View().Traces[traceID]; trace != nil && trace.State == want {
			return
		}
		goruntime.Gosched()
	}
	t.Fatalf("trace %s state=%v want=%s", traceID, session.rt.manager.View().Traces[traceID], want)
}

func TestP2TicketSessionOperationsUsesClaimedTicket(t *testing.T) {
	workspace := t.TempDir()
	backend := &sessionControlledProcess{releases: map[string]chan struct{}{}}
	definition := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "process-operations", Effect: "write", Resources: []agent.ExecutionResource{{Identity: "shared-file"}}, Argv: []string{"test"}}}
	model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}})
	session, err := CreateAgentSession(t.Context(), Options{Workspace: workspace, StateRoot: "memory", SessionID: "ticket-revoked", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{definition}, Operations: tools.Operations{Process: backend}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	receipt, err := session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"work"}`)})
	if err != nil {
		t.Fatal(err)
	}
	// The lower-level ticket test supplies the deterministic post-claim policy
	// revocation barrier. This session test verifies normal session wiring keeps
	// policy/ticket validation ahead of the Operations call.
	waitSessionTrace(t, session, receipt.TraceID, "completed")
	view := session.rt.manager.View()
	if backend.calls.Load() != 1 || backend.starts.Load() != 1 || view.Traces[receipt.TraceID].Usage.ToolExecutions != 1 {
		t.Fatalf("calls=%d starts=%d usage=%+v", backend.calls.Load(), backend.starts.Load(), view.Traces[receipt.TraceID].Usage)
	}
}
