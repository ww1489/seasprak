package eino

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"
)

const p3CheckpointCallbackChildEnv = "SEASPRAK_STEP8_CHECKPOINT_CALLBACK_CHILD"

// Global handlers are installed once in each isolated child, before any Runner
// executes. No test clears, replaces or restores the process-wide handler list.
func p3CheckpointCallbackSubprocess(t *testing.T, testName, mode string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^"+testName+"$", "-test.count=1", "-test.v", "-test.timeout=45s")
	cmd.Env = append(os.Environ(), p3CheckpointCallbackChildEnv+"="+mode)
	output, err := cmd.CombinedOutput()
	t.Logf("child %s output:\n%s", mode, output)
	if ctx.Err() != nil {
		t.Fatalf("child %s did not complete: %v", mode, ctx.Err())
	}
	// A callback panic that crashes the child is a regression failure, never a
	// successful isolation result. The valid-checkpoint assertions must finish.
	if err != nil {
		t.Fatalf("child %s must complete successfully: %v", mode, err)
	}
}

type p3CheckpointCallbackCounts struct {
	Needed  int64
	Timings [5]int64
}

func (c p3CheckpointCallbackCounts) minus(before p3CheckpointCallbackCounts) p3CheckpointCallbackCounts {
	c.Needed -= before.Needed
	for i := range c.Timings {
		c.Timings[i] -= before.Timings[i]
	}
	return c
}

type p3CheckpointCallbackHandler struct {
	needed  atomic.Int64
	timings [5]atomic.Int64
	armed   atomic.Bool
	panicAt string
	drains  sync.WaitGroup
}

var _ callbacks.Handler = (*p3CheckpointCallbackHandler)(nil)
var _ callbacks.TimingChecker = (*p3CheckpointCallbackHandler)(nil)

func (h *p3CheckpointCallbackHandler) snapshot() p3CheckpointCallbackCounts {
	c := p3CheckpointCallbackCounts{Needed: h.needed.Load()}
	for i := range c.Timings {
		c.Timings[i] = h.timings[i].Load()
	}
	return c
}

func (h *p3CheckpointCallbackHandler) failIfArmed(timing string) {
	if h.armed.Load() && h.panicAt == timing {
		fmt.Printf("armed callback %s reached: %+v\n", timing, h.snapshot())
		panic("synthetic Step8 lifecycle callback panic")
	}
}

func (h *p3CheckpointCallbackHandler) Needed(ctx context.Context, _ *callbacks.RunInfo, timing callbacks.CallbackTiming) bool {
	h.needed.Add(1)
	if normal, ok := ctx.Value(p3CheckpointNormalScopeKey{}).(*p3CheckpointNormalScope); ok {
		normal.needed.Add(1)
		if timing == callbacks.TimingOnStart && normal.initialized != nil {
			normal.once.Do(func() { close(normal.initialized) })
			<-normal.release
		}
	}
	h.failIfArmed("needed-panic")
	return true
}

func (h *p3CheckpointCallbackHandler) recordTiming(ctx context.Context, timing callbacks.CallbackTiming) {
	h.timings[timing].Add(1)
	if normal, ok := ctx.Value(p3CheckpointNormalScopeKey{}).(*p3CheckpointNormalScope); ok {
		normal.timings[timing].Add(1)
	}
}

func (h *p3CheckpointCallbackHandler) OnStart(ctx context.Context, _ *callbacks.RunInfo, _ callbacks.CallbackInput) context.Context {
	h.recordTiming(ctx, callbacks.TimingOnStart)
	h.failIfArmed("start-panic")
	return ctx
}

func (h *p3CheckpointCallbackHandler) OnEnd(ctx context.Context, _ *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	h.recordTiming(ctx, callbacks.TimingOnEnd)
	h.failIfArmed("end-panic")
	if out := adk.ConvTypedCallbackOutput[*schema.AgenticMessage](output); out != nil && out.Events != nil {
		h.drains.Add(1)
		go func() {
			defer h.drains.Done()
			for {
				if _, ok := out.Events.Next(); !ok {
					return
				}
			}
		}()
	}
	return ctx
}

func (h *p3CheckpointCallbackHandler) OnError(ctx context.Context, _ *callbacks.RunInfo, _ error) context.Context {
	h.recordTiming(ctx, callbacks.TimingOnError)
	h.failIfArmed("error-panic")
	return ctx
}

func (h *p3CheckpointCallbackHandler) OnStartWithStreamInput(ctx context.Context, _ *callbacks.RunInfo, in *schema.StreamReader[callbacks.CallbackInput]) context.Context {
	h.recordTiming(ctx, callbacks.TimingOnStartWithStreamInput)
	defer in.Close()
	h.failIfArmed("stream-start-panic")
	return ctx
}

func (h *p3CheckpointCallbackHandler) OnEndWithStreamOutput(ctx context.Context, _ *callbacks.RunInfo, out *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
	h.recordTiming(ctx, callbacks.TimingOnEndWithStreamOutput)
	defer out.Close()
	h.failIfArmed("stream-end-panic")
	return ctx
}

func p3CheckpointCallbackRegister(mode string) *p3CheckpointCallbackHandler {
	h := &p3CheckpointCallbackHandler{panicAt: mode}
	callbacks.AppendGlobalHandlers(h)
	return h
}

func TestP3CheckpointValidationProductionCallbacksZero(t *testing.T) {
	const name = "TestP3CheckpointValidationProductionCallbacksZero"
	mode := os.Getenv(p3CheckpointCallbackChildEnv)
	if !strings.HasPrefix(mode, "production:") {
		for _, timing := range []string{"count", "start-panic", "end-panic", "needed-panic"} {
			t.Run(timing, func(t *testing.T) {
				p3CheckpointCallbackSubprocess(t, name, "production:"+timing)
			})
		}
		return
	}
	handler := p3CheckpointCallbackRegister(strings.TrimPrefix(mode, "production:"))
	h := newCheckpointHarness(true)
	t.Cleanup(h.closeRelease)
	loop, cancel := h.startLoop(t, false, "")
	if ok, _ := loop.Push(checkpointPrompt); !ok {
		t.Fatal("Push rejected")
	}
	h.waitSignal(t, loop, cancel, h.model.started, "model did not enter")
	loop.Stop(adk.WithGraceful())
	exit := h.waitLoop(t, loop, cancel)
	assertCancelCheckpoint(t, exit, h.store, h.cpID)
	data, ok, err := h.store.Get(t.Context(), h.cpID)
	if err != nil || !ok {
		t.Fatal("actual paused checkpoint is missing")
	}
	handler.drains.Wait()
	beforeCallbacks := handler.snapshot()
	beforeBudget := h.budget.Snapshot()
	handler.armed.Store(true)
	err = ValidatePausedCheckpoint(data, checkpointPrompt, "main")
	handler.drains.Wait()
	delta := handler.snapshot().minus(beforeCallbacks)
	t.Logf("production validation: error=%v callback_delta=%+v model=%d tools=%d", err, delta, h.fake.Calls(), h.tool.total())
	if err != nil {
		t.Errorf("valid checkpoint must pass even with hostile lifecycle callbacks: %v", err)
	}
	if delta != (p3CheckpointCallbackCounts{}) {
		t.Errorf("paused checkpoint validation must invoke no lifecycle callback or TimingChecker.Needed: %+v", delta)
	}
	if h.fake.Calls() != 1 || h.model.before.Load() != 1 || h.model.later.Load() != 0 || h.tool.total() != 0 || h.budget.Snapshot() != beforeBudget {
		t.Error("checkpoint validation repeated original business execution or changed its budget")
	}
	after, ok, getErr := h.store.Get(t.Context(), h.cpID)
	if getErr != nil || !ok || !bytes.Equal(data, after) {
		t.Error("checkpoint validation modified the original stored checkpoint")
	}
}
