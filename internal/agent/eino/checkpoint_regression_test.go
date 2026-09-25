package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/testkit"
)

// These tests probe native TurnLoop GenResume against product NewAgent.
// They use agent.InputRef and a restored BudgetLedger as the session layer would
// after a process reopen. They do not prove JSONL recovery or activity duration.

var checkpointPrompt = agent.InputRef{InputID: "in-1", TraceID: "tr", Kind: "prompt"}

func TestCheckpointResumeAfterModelBeforeTools(t *testing.T) {
	h := newCheckpointHarness(true)
	t.Cleanup(h.closeRelease)
	loop, cancel := h.startLoop(t, false, "")
	if pushed, _ := loop.Push(checkpointPrompt); !pushed {
		t.Fatal("initial Push rejected")
	}
	h.waitSignal(t, loop, cancel, h.model.started, "model did not enter before Stop")
	if n := h.tool.total(); n != 0 {
		t.Fatalf("tool ran %d times before the model returned", n)
	}
	loop.Stop(adk.WithGraceful())
	exit := h.waitLoop(t, loop, cancel)

	assertCancelCheckpoint(t, exit, h.store, h.cpID)
	assertInterruptedRef(t, exit.InterruptedItems, checkpointPrompt)
	if h.genInput.Load() != 1 || h.genResume.Load() != 0 {
		t.Fatalf("first segment GenInput=%d GenResume=%d, want 1/0", h.genInput.Load(), h.genResume.Load())
	}
	if h.model.before.Load() != 1 || h.model.later.Load() != 0 || h.fake.Calls() != 1 {
		t.Fatalf("first-segment model before=%d after=%d fake=%d, want 1/0/1 (model was repeated or skipped)", h.model.before.Load(), h.model.later.Load(), h.fake.Calls())
	}
	if n := h.tool.total(); n != 0 {
		t.Fatalf("tool ran %d times after graceful stop at the model safe point; CancelAfterChatModel did not hold tools", n)
	}
	assertUsage(t, h.budget, 1, 1, 0)
	prepared, finished, _ := h.boundary.snapshot()
	if len(prepared) != 1 || prepared[0] != "turn-1" {
		t.Fatalf("prepared turns=%v, want [turn-1]", prepared)
	}
	if len(finished) != 0 {
		t.Fatalf("FinishTurn ran before tools: %+v", finished)
	}

	prev := h.rebuildBudget()
	if prev == h.budget {
		t.Fatal("resume reused the original BudgetLedger instance")
	}
	assertUsage(t, h.budget, 1, 1, 0)
	h.resumed.Store(true)
	resume, resumeCancel := h.startLoop(t, true, prepared[0])
	exit2 := h.waitLoop(t, resume, resumeCancel)
	if exit2.ExitReason != nil {
		t.Fatalf("resume ExitReason=%v", exit2.ExitReason)
	}
	if h.genInput.Load() != 1 || h.genResume.Load() != 1 {
		t.Fatalf("resume GenInput=%d GenResume=%d, want 1/1 (missing runner-state GenResume is not a true resume)", h.genInput.Load(), h.genResume.Load())
	}
	assertInterruptedRef(t, h.resumeInterrupted, checkpointPrompt)
	if h.model.before.Load() != 1 || h.model.later.Load() != 1 || h.fake.Calls() != 2 {
		t.Fatalf("resume model before=%d after=%d fake=%d, want 1/1/2 (first model must not run again)", h.model.before.Load(), h.model.later.Load(), h.fake.Calls())
	}
	if h.tool.before.Load() != 0 || h.tool.later.Load() != 1 {
		t.Fatalf("resume tool before=%d after=%d, want 0/1 (original tool must run once on GenResume)", h.tool.before.Load(), h.tool.later.Load())
	}
	if !h.model.sawToolResult.Load() {
		t.Fatal("follow-up model did not see a tool result; resume re-prompted instead of continuing the checkpoint")
	}
	assertUsage(t, h.budget, 2, 2, 1)
	assertUsage(t, prev, 1, 1, 0)
	assertToolTurn(t, h.tool, "turn-1")
	_, finished, scopes := h.boundary.snapshot()
	if len(finished) < 1 || !finished[0].HasTools || finished[0].TurnID != "turn-1" || scopes[0].TurnID != "turn-1" {
		t.Fatalf("FinishAfterTools identity fact=%+v scopes=%+v", finished, scopes)
	}
}

func TestCheckpointResumeAfterToolGracefulStop(t *testing.T) {
	h := newCheckpointHarness(false)
	t.Cleanup(h.closeRelease)
	loop, cancel := h.startLoop(t, false, "")
	if pushed, _ := loop.Push(checkpointPrompt); !pushed {
		t.Fatal("initial Push rejected")
	}
	h.waitSignal(t, loop, cancel, h.tool.started, "tool did not enter before Stop")
	loop.Stop(adk.WithGraceful())
	exit := h.waitLoop(t, loop, cancel)

	assertCancelCheckpoint(t, exit, h.store, h.cpID)
	assertInterruptedRef(t, exit.InterruptedItems, checkpointPrompt)
	if h.genInput.Load() != 1 || h.genResume.Load() != 0 {
		t.Fatalf("first segment GenInput=%d GenResume=%d, want 1/0", h.genInput.Load(), h.genResume.Load())
	}
	if h.model.before.Load() != 1 || h.fake.Calls() != 1 {
		t.Fatalf("first-segment model before=%d fake=%d, want 1/1", h.model.before.Load(), h.fake.Calls())
	}
	if h.tool.before.Load() != 1 || h.tool.later.Load() != 0 {
		t.Fatalf("first-segment tool before=%d after=%d, want 1/0", h.tool.before.Load(), h.tool.later.Load())
	}
	assertUsage(t, h.budget, 1, 1, 1)
	prepared, finished, scopes := h.boundary.snapshot()
	if len(prepared) != 1 || prepared[0] != "turn-1" {
		t.Fatalf("prepared turns=%v, want [turn-1]", prepared)
	}
	if len(finished) != 1 || !finished[0].HasTools || finished[0].TurnID != "turn-1" || scopes[0].TurnID != "turn-1" {
		t.Fatalf("expected FinishAfterTools before pause fact=%+v scopes=%+v", finished, scopes)
	}

	prev := h.rebuildBudget()
	if prev == h.budget {
		t.Fatal("resume reused the original BudgetLedger instance")
	}
	assertUsage(t, h.budget, 1, 1, 1)
	h.resumed.Store(true)
	resume, resumeCancel := h.startLoop(t, true, prepared[0])
	exit2 := h.waitLoop(t, resume, resumeCancel)
	if exit2.ExitReason != nil {
		t.Fatalf("resume ExitReason=%v", exit2.ExitReason)
	}
	if h.genInput.Load() != 1 || h.genResume.Load() != 1 {
		t.Fatalf("resume GenInput=%d GenResume=%d, want 1/1 (missing runner-state GenResume is not a true resume)", h.genInput.Load(), h.genResume.Load())
	}
	assertInterruptedRef(t, h.resumeInterrupted, checkpointPrompt)
	if h.model.before.Load() != 1 || h.model.later.Load() != 1 || h.fake.Calls() != 2 {
		t.Fatalf("resume model before=%d after=%d fake=%d, want 1/1/2 (first model must not run again)", h.model.before.Load(), h.model.later.Load(), h.fake.Calls())
	}
	if h.tool.before.Load() != 1 || h.tool.later.Load() != 0 || h.tool.total() != 1 {
		t.Fatalf("resume re-executed the tool before=%d after=%d total=%d", h.tool.before.Load(), h.tool.later.Load(), h.tool.total())
	}
	if !h.model.sawToolResult.Load() {
		t.Fatal("follow-up model did not see the checkpointed tool result; tool output was not reused")
	}
	assertUsage(t, h.budget, 2, 2, 1)
	assertUsage(t, prev, 1, 1, 1)
	assertToolTurn(t, h.tool, "turn-1")
}

type memoryStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *memoryStore) Get(_ context.Context, id string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[id]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), b...), true, nil
}

func (s *memoryStore) Set(_ context.Context, id string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	s.m[id] = append([]byte(nil), data...)
	return nil
}

type checkpointSink struct {
	mu    sync.Mutex
	facts []agent.Fact
}

func (s *checkpointSink) CommitFact(_ context.Context, _ agent.ExecutionScope, fact agent.Fact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.facts = append(s.facts, fact)
	return nil
}

type checkpointBoundary struct {
	mu       sync.Mutex
	seq      int
	prepared []string
	finished []agent.TurnFact
	scopes   []agent.ExecutionScope
}

func (b *checkpointBoundary) PrepareNextTurn(context.Context, agent.ExecutionScope) (agent.TurnPlan, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	id := fmt.Sprintf("turn-%d", b.seq)
	b.prepared = append(b.prepared, id)
	return agent.TurnPlan{TurnID: id}, nil
}

func (b *checkpointBoundary) ShouldStop(context.Context, agent.ExecutionScope) (bool, string, error) {
	return false, "", nil
}

func (b *checkpointBoundary) FinishTurn(_ context.Context, scope agent.ExecutionScope, fact agent.TurnFact) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.finished = append(b.finished, fact)
	b.scopes = append(b.scopes, scope)
	return nil
}

func (b *checkpointBoundary) TakeSteering(context.Context, agent.ExecutionScope) (*agent.AgentMessage, error) {
	return nil, nil
}

func (b *checkpointBoundary) snapshot() (prepared []string, finished []agent.TurnFact, scopes []agent.ExecutionScope) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.prepared...), append([]agent.TurnFact(nil), b.finished...), append([]agent.ExecutionScope(nil), b.scopes...)
}

type phaseModel struct {
	inner         *testkit.FakeModel
	started       chan struct{}
	after         *atomic.Bool
	before        atomic.Int32
	later         atomic.Int32
	sawToolResult atomic.Bool
	once          sync.Once
}

func (m *phaseModel) note(in []*schema.AgenticMessage) {
	m.once.Do(func() { close(m.started) })
	if m.after.Load() {
		m.later.Add(1)
		if messagesHaveToolResult(in) {
			m.sawToolResult.Store(true)
		}
		return
	}
	m.before.Add(1)
}

func (m *phaseModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	m.note(in)
	return m.inner.Generate(ctx, in, opts...)
}

func (m *phaseModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.note(in)
	return m.inner.Stream(ctx, in, opts...)
}

type phaseTool struct {
	started chan struct{}
	release chan struct{}
	after   *atomic.Bool
	before  atomic.Int32
	later   atomic.Int32
	once    sync.Once
	mu      sync.Mutex
	turns   []string
}

func (p *phaseTool) total() int32 { return p.before.Load() + p.later.Load() }

func (p *phaseTool) run(ctx context.Context, _ json.RawMessage) (string, error) {
	if p.after.Load() {
		p.later.Add(1)
	} else {
		p.before.Add(1)
	}
	p.mu.Lock()
	p.turns = append(p.turns, ScopeFromContext(ctx, agent.ExecutionScope{}).TurnID)
	p.mu.Unlock()
	p.once.Do(func() { close(p.started) })
	if p.release == nil {
		return "1", nil
	}
	select {
	case <-p.release:
		return "1", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type checkpointHarness struct {
	store             *memoryStore
	cpID              string
	scope             agent.ExecutionScope
	budget            *agent.BudgetLedger
	boundary          *checkpointBoundary
	sink              *checkpointSink
	fake              *testkit.FakeModel
	model             *phaseModel
	tool              *phaseTool
	genInput          atomic.Int32
	genResume         atomic.Int32
	resumed           atomic.Bool
	release           chan struct{}
	releaseOnce       sync.Once
	resumeInterrupted []agent.InputRef
}

func newCheckpointHarness(gateModel bool) *checkpointHarness {
	release := make(chan struct{})
	h := &checkpointHarness{
		store:    &memoryStore{m: map[string][]byte{}},
		cpID:     "native-checkpoint",
		scope:    agent.ExecutionScope{SessionID: "sess", TraceID: checkpointPrompt.TraceID, InvocationID: "inv", Generation: "gen", ExecutionID: "exec-1"},
		budget:   agent.NewBudget(config.DefaultLimits()),
		boundary: &checkpointBoundary{},
		sink:     &checkpointSink{},
		release:  release,
	}
	step := testkit.Step{
		Finish:    "tool_calls",
		ToolCalls: []schema.FunctionToolCall{{CallID: "p1", Name: "add", Arguments: `{"n":1}`}},
	}
	if gateModel {
		step.Gate = release
	}
	h.fake = testkit.NewFake(step, testkit.Step{Text: "done"})
	h.model = &phaseModel{inner: h.fake, started: make(chan struct{}), after: &h.resumed}
	h.tool = &phaseTool{started: make(chan struct{}), after: &h.resumed}
	if !gateModel {
		h.tool.release = release
	}
	return h
}

func (h *checkpointHarness) closeRelease() {
	if h.release == nil {
		return
	}
	h.releaseOnce.Do(func() { close(h.release) })
}

func (h *checkpointHarness) rebuildBudget() *agent.BudgetLedger {
	prev := h.budget
	next := agent.NewBudget(config.DefaultLimits())
	next.Restore(prev.Snapshot())
	h.budget = next
	return prev
}

func (h *checkpointHarness) startLoop(t *testing.T, resume bool, firstTurn string) (*adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	loop := h.newLoop(resume, firstTurn)
	t.Cleanup(func() { h.recycle(t, loop, cancel, nil) })
	loop.Run(ctx)
	return loop, cancel
}

func (h *checkpointHarness) newLoop(resume bool, firstTurn string) *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage] {
	scope := h.scope
	if resume {
		scope.TurnID = firstTurn
		scope.ExecutionID = "exec-2"
	}
	return adk.NewTurnLoop(adk.TurnLoopConfig[agent.InputRef, *schema.AgenticMessage]{
		Store:        h.store,
		CheckpointID: h.cpID,
		GenInput: func(ctx context.Context, _ *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], items []agent.InputRef) (*adk.GenInputResult[agent.InputRef, *schema.AgenticMessage], error) {
			h.genInput.Add(1)
			if resume {
				return nil, errors.New("GenInput called on resume loop; checkpoint had no runner state so GenResume was skipped")
			}
			if len(items) == 0 {
				return nil, errors.New("empty turn items")
			}
			return &adk.GenInputResult[agent.InputRef, *schema.AgenticMessage]{
				RunCtx:    WithExecutionScope(ctx, scope),
				Input:     &adk.TypedAgentInput[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{schema.UserAgenticMessage(items[0].InputID)}},
				Consumed:  items[:1],
				Remaining: items[1:],
				RunOpts:   []adk.AgentRunOption{adk.WithAfterToolCallsHook(FinishAfterTools(h.boundary, scope))},
			}, nil
		},
		GenResume: func(ctx context.Context, _ *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], interrupted, unhandled, newItems []agent.InputRef) (*adk.GenResumeResult[agent.InputRef, *schema.AgenticMessage], error) {
			h.genResume.Add(1)
			if !resume {
				return nil, errors.New("GenResume called on a fresh loop")
			}
			h.resumeInterrupted = append([]agent.InputRef(nil), interrupted...)
			if err := inputRefIdentity(interrupted, checkpointPrompt); err != nil {
				return nil, err
			}
			return &adk.GenResumeResult[agent.InputRef, *schema.AgenticMessage]{
				RunCtx:    WithExecutionScope(ctx, scope),
				RunOpts:   []adk.AgentRunOption{adk.WithAfterToolCallsHook(FinishAfterTools(h.boundary, scope))},
				Consumed:  interrupted,
				Remaining: append(append([]agent.InputRef{}, unhandled...), newItems...),
			}, nil
		},
		PrepareAgent: func(context.Context, *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], []agent.InputRef) (adk.TypedAgent[*schema.AgenticMessage], error) {
			exec, err := tools.NewExecutor("gen", []tools.Definition{{
				Version: "1", Name: "add",
				Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
				Run:    h.tool.run,
			}}, h.sink, allowAll{}, h.budget)
			if err != nil {
				return nil, err
			}
			return NewAgent(context.Background(), Deps{
				Model: h.model, Tools: []tool.BaseTool{NewPipelineTool(testkit.ToolInfo("add", "add"), exec, scope)},
				Sink: h.sink, Budget: h.budget, Boundary: h.boundary, Instruction: "test", Scope: scope,
			})
		},
		OnAgentEvents: func(ctx context.Context, tc *adk.TurnContext[agent.InputRef, *schema.AgenticMessage], events *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]) error {
			if !resume && h.release != nil {
				go func() {
					select {
					case <-tc.Stopped:
						h.closeRelease()
					case <-ctx.Done():
					}
				}()
			}
			if err := drainCancelAware(events); err != nil {
				return err
			}
			tc.Loop.Stop()
			return nil
		},
	})
}

func (h *checkpointHarness) waitSignal(t *testing.T, loop *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], cancel context.CancelFunc, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(8 * time.Second):
		h.recycle(t, loop, cancel, nil)
		t.Fatal(msg)
	}
}

func (h *checkpointHarness) waitLoop(t *testing.T, loop *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], cancel context.CancelFunc) *adk.TurnLoopExitState[agent.InputRef, *schema.AgenticMessage] {
	t.Helper()
	done := make(chan *adk.TurnLoopExitState[agent.InputRef, *schema.AgenticMessage], 1)
	go func() { done <- loop.Wait() }()
	select {
	case exit := <-done:
		return exit
	case <-time.After(8 * time.Second):
		h.recycle(t, loop, cancel, done)
		t.Fatal("TurnLoop.Wait hung")
	}
	return nil
}

func (h *checkpointHarness) recycle(t *testing.T, loop *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], cancel context.CancelFunc, waiting <-chan *adk.TurnLoopExitState[agent.InputRef, *schema.AgenticMessage]) {
	t.Helper()
	h.closeRelease()
	cancel()
	loop.Stop(adk.WithImmediate())
	if waiting == nil {
		done := make(chan *adk.TurnLoopExitState[agent.InputRef, *schema.AgenticMessage], 1)
		go func() { done <- loop.Wait() }()
		waiting = done
	}
	select {
	case <-waiting:
	case <-time.After(8 * time.Second):
		t.Error("TurnLoop did not exit after test cleanup cancellation")
	}
}

func drainCancelAware(events *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]) error {
	var first error
	for {
		ev, ok := events.Next()
		if !ok {
			return first
		}
		if ev == nil || ev.Err == nil {
			continue
		}
		var cancelErr *adk.CancelError
		if errors.As(ev.Err, &cancelErr) {
			continue
		}
		if first == nil {
			first = ev.Err
		}
	}
}

func assertCancelCheckpoint(t *testing.T, exit *adk.TurnLoopExitState[agent.InputRef, *schema.AgenticMessage], store *memoryStore, id string) {
	t.Helper()
	var cancelErr *adk.CancelError
	if !errors.As(exit.ExitReason, &cancelErr) {
		t.Fatalf("ExitReason=%v, want *adk.CancelError from graceful Stop", exit.ExitReason)
	}
	if !exit.CheckpointAttempted {
		t.Fatal("CheckpointAttempted=false; TurnLoop did not persist runner state")
	}
	if exit.CheckpointErr != nil {
		t.Fatalf("CheckpointErr=%v", exit.CheckpointErr)
	}
	blob, ok, err := store.Get(context.Background(), id)
	if err != nil || !ok || len(blob) == 0 {
		t.Fatalf("checkpoint blob missing: ok=%v err=%v len=%d", ok, err, len(blob))
	}
}

func assertInterruptedRef(t *testing.T, got []agent.InputRef, want agent.InputRef) {
	t.Helper()
	if err := inputRefIdentity(got, want); err != nil {
		t.Fatal(err)
	}
}

func inputRefIdentity(got []agent.InputRef, want agent.InputRef) error {
	if len(got) != 1 {
		return fmt.Errorf("InterruptedItems=%v, want one InputRef", got)
	}
	if got[0].InputID != want.InputID || got[0].TraceID != want.TraceID || got[0].Kind != want.Kind {
		return fmt.Errorf("InputRef %+v, want InputID=%s TraceID=%s Kind=%s", got[0], want.InputID, want.TraceID, want.Kind)
	}
	return nil
}

func assertUsage(t *testing.T, budg *agent.BudgetLedger, logical, transport, toolExec int) {
	t.Helper()
	u := budg.Snapshot()
	if u.LogicalModelCalls != logical || u.TransportRequests != transport || u.ToolExecutions != toolExec {
		t.Fatalf("budget %+v, want logical=%d transport=%d tools=%d", u, logical, transport, toolExec)
	}
}

func assertToolTurn(t *testing.T, p *phaseTool, want string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.turns) == 0 {
		t.Fatal("tool ran with no recorded turn")
	}
	for _, id := range p.turns {
		if id != want {
			t.Fatalf("tool turn IDs=%v, want all %s", p.turns, want)
		}
	}
}

func messagesHaveToolResult(in []*schema.AgenticMessage) bool {
	for _, msg := range in {
		if msg == nil {
			continue
		}
		for _, block := range msg.ContentBlocks {
			if block != nil && block.Type == schema.ContentBlockTypeFunctionToolResult {
				return true
			}
		}
	}
	return false
}
