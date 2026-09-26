package eino

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/testkit"
)

// Framework-only probes: native tools deliberately bypass the product executor.
// checkpointSink has no LookupTool, so successful A can only be reused by Eino's
// checkpoint, not by product result deduplication. These are not ask_user or
// durable session-answer acceptance tests.
func TestP2FrameworkBatchBusinessInterrupt(t *testing.T) {
	h := newP2BatchProbe()
	r := h.start(t, false, nil, false)
	r.loop.Push(checkpointPrompt)
	p2Await(t, h.askEntered, "B entered after A completed")
	h.release()
	exit := r.wait(t)
	target := h.assertAskCheckpoint(t, exit, false)
	h.assertCounts(t, 1, 0, 1, 0)
	h.resumeAnswer(t, target)
}

func TestP2FrameworkBatchGracefulAskCompetition(t *testing.T) {
	for _, afterAsk := range []bool{false, true} {
		t.Run(fmt.Sprintf("stop_after_ask_%t", afterAsk), func(t *testing.T) {
			h := newP2BatchProbe()
			r := h.start(t, false, nil, afterAsk)
			r.loop.Push(checkpointPrompt)
			p2Await(t, h.askEntered, "B entered after A completed")
			if afterAsk {
				h.release()
				p2Await(t, r.askObserved, "business interrupt observed")
			}
			r.loop.Stop(adk.WithGraceful())
			if !afterAsk {
				p2Await(t, r.ready, "event callback ready")
				p2Await(t, r.stopped, "graceful signal applied")
			}
			h.release()
			r.releaseEvents()
			exit := r.wait(t)
			// Once the business interrupt has been emitted, Stop cannot
			// retroactively turn it into CancelError.
			target := h.assertAskCheckpoint(t, exit, !afterAsk)
			h.assertCounts(t, 1, 0, 1, 0)
			h.resumeAnswer(t, target)
		})
	}
}

// Native graph interruption is deliberately characterized separately from the
// product abort contract. A finished runner is not proof its tools have exited.
func TestP2FrameworkNativeImmediateOutlivesTool(t *testing.T) {
	h := newP2BatchProbe()
	r := h.start(t, false, nil, false)
	if ok, _ := r.loop.Push(checkpointPrompt); !ok {
		t.Fatal("Push rejected")
	}
	p2Await(t, h.askEntered, "B started")
	t.Cleanup(func() {
		r.cancel()
		select {
		case <-h.askCanceled:
		case <-time.After(8 * time.Second):
			t.Error("native interrupted tool failed to exit after parent cancellation")
		}
	})
	r.loop.Stop(adk.WithImmediate())
	exit := r.wait(t)
	var ce *adk.CancelError
	if !errors.As(exit.ExitReason, &ce) {
		t.Fatalf("native exit=%v", exit.ExitReason)
	}
	select {
	case <-h.askCanceled:
		t.Fatal("native context cancellation behavior changed; reassess the adapter")
	default:
	}
	h.assertCounts(t, 1, 0, 1, 0)
	// The original native safety failure is retained as a positive observation:
	// Wait has returned, but B still needs explicit parent context cancellation.
	r.cancel()
	p2Await(t, h.askCanceled, "native B exited after explicit context cancellation")
}

func TestP2FrameworkBatchAbortSafety(t *testing.T) {
	for _, immediate := range []bool{false, true} {
		t.Run(fmt.Sprintf("immediate_%t", immediate), func(t *testing.T) {
			h := newP2BatchProbe()
			r := h.start(t, false, nil, false)
			r.loop.Push(checkpointPrompt)
			p2Await(t, h.askEntered, "B entered after A completed")
			if immediate {
				AbortTurnLoop(r.loop, r.cancel)
			} else {
				r.cancel()
			}
			h.assertCounts(t, 1, 0, 1, 0)
			// Native WithImmediate alone fails this safety expectation in
			// v0.9.15 and v0.9.21. The product adapter also cancels context.
			select {
			case <-h.askCanceled:
			case <-time.After(8 * time.Second):
				t.Error("blocked B did not receive context cancellation after applied stop")
				return
			}
			exit := r.wait(t)
			h.assertCounts(t, 1, 0, 1, 0)
			if h.model.Calls() != 1 || h.bCalls.Load() != 1 {
				t.Fatal("abort repeated a model or tool call")
			}
			if immediate {
				var ce *adk.CancelError
				if !errors.Is(exit.ExitReason, context.Canceled) && !errors.As(exit.ExitReason, &ce) {
					t.Fatalf("abort exit=%v, want cancellation", exit.ExitReason)
				}
				if exit.CheckpointAttempted || exit.CheckpointErr != nil {
					t.Fatalf("abort must not publish a resumable checkpoint: attempted=%v err=%v", exit.CheckpointAttempted, exit.CheckpointErr)
				}
				if _, ok, _ := h.store.Get(context.Background(), "p2-batch"); ok {
					t.Fatal("abort created a checkpoint")
				}
			} else {
				if !errors.Is(exit.ExitReason, context.Canceled) {
					t.Fatalf("context cancellation exit=%v", exit.ExitReason)
				}
				if exit.CheckpointAttempted {
					t.Fatal("plain context cancellation unexpectedly attempted a checkpoint")
				}
				if _, ok, _ := h.store.Get(context.Background(), "p2-batch"); ok {
					t.Fatal("plain context cancellation unexpectedly saved a checkpoint")
				}
			}
		})
	}
}

type p2ProbeTool struct {
	name string
	run  func(context.Context) (string, error)
}

func (p *p2ProbeTool) Info(context.Context) (*schema.ToolInfo, error) {
	return testkit.ToolInfo(p.name, p.name), nil
}
func (p *p2ProbeTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	return p.run(ctx)
}

type p2BatchModel struct{ *testkit.FakeModel }

func (m *p2BatchModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	if m.Calls() != 0 {
		seen := map[string]int{}
		for _, msg := range in {
			for _, b := range msg.ContentBlocks {
				if b.FunctionToolResult != nil {
					seen[b.FunctionToolResult.CallID]++
				}
			}
		}
		if seen["original-A"] != 1 || seen["original-B"] != 1 {
			return nil, fmt.Errorf("follow-up model lost original tool results: %v", seen)
		}
	}
	return m.FakeModel.Generate(ctx, in, opts...)
}

type p2BatchProbe struct {
	streaming                                     bool
	toolsOverride                                 []tool.BaseTool
	store                                         *memoryStore
	sink                                          *checkpointSink
	boundary                                      *checkpointBoundary
	budget                                        *agent.BudgetLedger
	model                                         *p2BatchModel
	aDone, askEntered, askCanceled, gate          chan struct{}
	aOnce, askOnce, cancelOnce, releaseOnce       sync.Once
	aCalls, bCalls, bEffects, genInput, genResume atomic.Int32
}

func newP2BatchProbe() *p2BatchProbe {
	return &p2BatchProbe{
		store: &memoryStore{}, sink: &checkpointSink{}, boundary: &checkpointBoundary{},
		budget: agent.NewBudget(config.DefaultLimits()),
		model: &p2BatchModel{testkit.NewFake(testkit.Step{Finish: "tool_calls", ToolCalls: []schema.FunctionToolCall{
			{CallID: "original-A", Name: "A", Arguments: `{}`},
			{CallID: "original-B", Name: "B", Arguments: `{}`},
		}}, testkit.Step{Text: "done"})},
		aDone: make(chan struct{}), askEntered: make(chan struct{}), askCanceled: make(chan struct{}), gate: make(chan struct{}),
	}
}

func (h *p2BatchProbe) release() { h.releaseOnce.Do(func() { close(h.gate) }) }

func (h *p2BatchProbe) tools() []tool.BaseTool {
	if h.toolsOverride != nil {
		return h.toolsOverride
	}
	return []tool.BaseTool{
		&p2ProbeTool{name: "A", run: func(ctx context.Context) (string, error) {
			h.aCalls.Add(1)
			if compose.GetToolCallID(ctx) != "original-A" {
				return "", errors.New("A CallID changed")
			}
			h.aOnce.Do(func() { close(h.aDone) })
			return "A success", nil
		}},
		&p2ProbeTool{name: "B", run: func(ctx context.Context) (string, error) {
			h.bCalls.Add(1)
			if compose.GetToolCallID(ctx) != "original-B" {
				return "", errors.New("B CallID changed")
			}
			select {
			case <-h.aDone:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			was, hasState, state := tool.GetInterruptState[string](ctx)
			if was && (!hasState || state != "original-B") {
				return "", fmt.Errorf("B lost interrupt state: %t %q", hasState, state)
			}
			target, hasData, answer := tool.GetResumeContext[string](ctx)
			if was && target && hasData && answer == "approved" {
				h.bEffects.Add(1)
				return "B approved", nil
			}
			h.askOnce.Do(func() { close(h.askEntered) })
			select {
			case <-h.gate:
				return "", tool.StatefulInterrupt(ctx, "approve B", "original-B")
			case <-ctx.Done():
				h.cancelOnce.Do(func() { close(h.askCanceled) })
				return "", ctx.Err()
			}
		}},
	}
}

type p2ProbeRun struct {
	loop                          *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage]
	cancel                        context.CancelFunc
	done                          chan struct{}
	exit                          *adk.TurnLoopExitState[agent.InputRef, *schema.AgenticMessage]
	stopped                       <-chan struct{}
	ready, askObserved, eventGate chan struct{}
	releaseOnce                   sync.Once
}

func (r *p2ProbeRun) releaseEvents() { r.releaseOnce.Do(func() { close(r.eventGate) }) }
func (r *p2ProbeRun) wait(t *testing.T) *adk.TurnLoopExitState[agent.InputRef, *schema.AgenticMessage] {
	t.Helper()
	p2Await(t, r.done, "TurnLoop exited")
	return r.exit
}

func (h *p2BatchProbe) start(t *testing.T, resume bool, answers map[string]any, holdEvent bool) *p2ProbeRun {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &p2ProbeRun{cancel: cancel, done: make(chan struct{}), askObserved: make(chan struct{}), eventGate: make(chan struct{})}
	ready := make(chan struct{})
	scope := agent.ExecutionScope{SessionID: "p2", TraceID: checkpointPrompt.TraceID, InvocationID: "inv", Generation: "gen", ExecutionID: "exec", TurnID: "turn-1"}
	r.loop = adk.NewTurnLoop(adk.TurnLoopConfig[agent.InputRef, *schema.AgenticMessage]{
		Store: h.store, CheckpointID: "p2-batch",
		GenInput: func(ctx context.Context, _ *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], items []agent.InputRef) (*adk.GenInputResult[agent.InputRef, *schema.AgenticMessage], error) {
			h.genInput.Add(1)
			if resume {
				return nil, errors.New("resume fell back to GenInput")
			}
			if err := inputRefIdentity(items, checkpointPrompt); err != nil {
				return nil, err
			}
			return &adk.GenInputResult[agent.InputRef, *schema.AgenticMessage]{RunCtx: WithExecutionScope(ctx, scope), Input: &adk.TypedAgentInput[*schema.AgenticMessage]{EnableStreaming: h.streaming, Messages: []*schema.AgenticMessage{schema.UserAgenticMessage("batch")}}, Consumed: items}, nil
		},
		GenResume: func(ctx context.Context, _ *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], interrupted, unhandled, newItems []agent.InputRef) (*adk.GenResumeResult[agent.InputRef, *schema.AgenticMessage], error) {
			h.genResume.Add(1)
			if !resume {
				return nil, errors.New("unexpected GenResume")
			}
			if err := inputRefIdentity(interrupted, checkpointPrompt); err != nil {
				return nil, err
			}
			if len(unhandled)+len(newItems) != 0 {
				return nil, errors.New("unexpected extra InputRef")
			}
			return &adk.GenResumeResult[agent.InputRef, *schema.AgenticMessage]{RunCtx: WithExecutionScope(ctx, scope), Consumed: interrupted, ResumeParams: &adk.ResumeParams{Targets: answers}}, nil
		},
		PrepareAgent: func(ctx context.Context, _ *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], _ []agent.InputRef) (adk.TypedAgent[*schema.AgenticMessage], error) {
			return NewAgent(ctx, Deps{Model: h.model, Tools: h.tools(), Sink: h.sink, Boundary: h.boundary, Budget: h.budget, Scope: scope, Instruction: "test batch"})
		},
		OnAgentEvents: func(ctx context.Context, tc *adk.TurnContext[agent.InputRef, *schema.AgenticMessage], events *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]) error {
			r.stopped = tc.Stopped
			close(ready)
			var first error
			for {
				ev, ok := events.Next()
				if !ok {
					break
				}
				if ev == nil {
					continue
				}
				if ev.Action != nil && ev.Action.Interrupted != nil && holdEvent {
					close(r.askObserved)
					select {
					case <-r.eventGate:
					case <-ctx.Done():
					}
				}
				if h.streaming && ev.Output != nil && ev.Output.MessageOutput != nil {
					// Drain owned event readers; runner errors remain authoritative.
					_, _ = ev.Output.MessageOutput.GetMessage()
				}
				if ev.Err != nil && first == nil {
					first = ev.Err
				}
			}
			// Do not turn a context-only abort into a Stop checkpoint.
			if ctx.Err() == nil {
				tc.Loop.Stop()
			}
			return first
		},
	})
	t.Cleanup(func() {
		cancel()
		h.release()
		r.releaseEvents()
		r.loop.Stop(adk.WithImmediate())
		select {
		case <-r.done:
		case <-time.After(8 * time.Second):
			t.Error("P2 TurnLoop cleanup did not join Wait")
		}
	})
	r.loop.Run(ctx)
	go func() { r.exit = r.loop.Wait(); close(r.done) }()
	r.ready = ready
	return r
}

func (h *p2BatchProbe) assertAskCheckpoint(t *testing.T, exit *adk.TurnLoopExitState[agent.InputRef, *schema.AgenticMessage], canceled bool) string {
	t.Helper()
	var contexts []*adk.InterruptCtx
	if canceled {
		var ce *adk.CancelError
		if !errors.As(exit.ExitReason, &ce) {
			t.Fatalf("exit=%v, want CancelError", exit.ExitReason)
		}
		contexts = ce.InterruptContexts
	} else {
		var ie *adk.InterruptError
		if !errors.As(exit.ExitReason, &ie) {
			t.Fatalf("exit=%v, want InterruptError", exit.ExitReason)
		}
		contexts = ie.InterruptContexts
	}
	if !exit.CheckpointAttempted || exit.CheckpointErr != nil {
		t.Fatalf("checkpoint attempted=%v err=%v", exit.CheckpointAttempted, exit.CheckpointErr)
	}
	if b, ok, err := h.store.Get(context.Background(), "p2-batch"); err != nil || !ok || len(b) == 0 {
		t.Fatalf("checkpoint missing: ok=%v err=%v", ok, err)
	}
	assertInterruptedRef(t, exit.InterruptedItems, checkpointPrompt)
	for _, ic := range contexts {
		if ic.Info == "approve B" && ic.IsRootCause {
			for _, seg := range ic.Address {
				if seg.Type == adk.AddressSegmentTool && seg.ID == "B" && seg.SubID == "original-B" {
					return ic.ID
				}
			}
			t.Fatalf("ask lost original CallID: %+v", ic.Address)
		}
	}
	t.Fatalf("business ask target missing: %+v", contexts)
	return ""
}

func (h *p2BatchProbe) assertCounts(t *testing.T, a, effects, input, resume int32) {
	t.Helper()
	if h.aCalls.Load() != a || h.bEffects.Load() != effects || h.genInput.Load() != input || h.genResume.Load() != resume {
		t.Fatalf("A=%d B effects=%d GenInput=%d GenResume=%d, want %d/%d/%d/%d", h.aCalls.Load(), h.bEffects.Load(), h.genInput.Load(), h.genResume.Load(), a, effects, input, resume)
	}
}

func (h *p2BatchProbe) resumeAnswer(t *testing.T, target string) {
	t.Helper()
	// No answer is not approval. Reopen from checkpoint and re-interrupt B.
	unanswered := h.start(t, true, nil, false).wait(t)
	latestTarget := h.assertAskCheckpoint(t, unanswered, false)
	// Eino creates a fresh interrupt UUID on re-interruption. The tool
	// address and original CallID stay stable; target the latest checkpoint.
	if latestTarget == "" || latestTarget == target {
		t.Fatalf("re-interruption did not allocate a fresh target: %q", latestTarget)
	}
	target = latestTarget
	h.assertCounts(t, 1, 0, 1, 1)
	if h.model.Calls() != 1 || h.bCalls.Load() != 2 {
		t.Fatalf("unanswered model=%d B entries=%d", h.model.Calls(), h.bCalls.Load())
	}
	// This map stands in for a server-owned saved answer record, never model
	// arguments. Durable persistence and validation belong to later P2 work.
	savedAnswers := map[string]any{target: "approved"}
	answered := h.start(t, true, savedAnswers, false).wait(t)
	if answered.ExitReason != nil {
		t.Fatalf("targeted resume exit=%v", answered.ExitReason)
	}
	h.assertCounts(t, 1, 1, 1, 2)
	if h.model.Calls() != 2 || h.bCalls.Load() != 3 {
		t.Fatalf("answered model=%d B entries=%d", h.model.Calls(), h.bCalls.Load())
	}
}

func p2Await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(8 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
}
