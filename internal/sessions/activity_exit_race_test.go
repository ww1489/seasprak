package sessions

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

type activityRenewalContextGate struct {
	context.Context
	armed            atomic.Bool
	entered, release chan struct{}
}

func (c *activityRenewalContextGate) Err() error {
	if c.armed.CompareAndSwap(true, false) {
		close(c.entered)
		<-c.release
	}
	return c.Context.Err()
}

// Exercise the production begin/endActivity boundary with the real mailbox and
// ledger. No model work is required: the race is after the worker's real exit.
func TestActivityRenewalRacingNormalExitDoesNotManufactureExpiry(t *testing.T) {
	st, err := memory.Open("activity-exit", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(st, "activity-exit")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := manager.AcceptWithLimits(t.Context(), agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"boundary"}`)}, agent.TargetAgent{Name: "main", Generation: "gen"}, config.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetTraceState(t.Context(), receipt.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	gate := &activityRenewalContextGate{Context: ctx, entered: make(chan struct{}), release: make(chan struct{})}
	clock := newManualActivityClock()
	frame := &execution{scope: agent.ExecutionScope{TraceID: receipt.TraceID, ExecutionID: "execution"}, ctx: gate, cancel: cancel, done: make(chan struct{})}
	rt := &runtime{manager: manager, clock: clock, active: frame, mailbox: make(chan command), done: make(chan struct{}), opts: Options{Store: st}}
	go rt.loop()
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
		_ = rt.do(context.Background(), func(rt *runtime) error { rt.active = nil; rt.closing = true; return nil })
		<-rt.done
	}()
	if err := rt.beginActivity(frame); err != nil {
		t.Fatal(err)
	}
	gate.armed.Store(true)
	clock.advance(500 * time.Millisecond)
	awaitActivitySignal(t, gate.entered)
	ended := make(chan error, 1)
	go func() { ended <- rt.endActivity(frame) }()
	waitResumeCondition(t, func() bool { frame.activity.mu.Lock(); defer frame.activity.mu.Unlock(); return frame.activity.closed })
	t.Log("renewal and normal exit fixed at 500ms, old deadline=1s; no model/backend invocations")
	if ctx.Err() != nil {
		t.Fatal("context cancelled before renewal resumed")
	}
	close(gate.release)
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("normal exit manufactured failure: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("endActivity did not join renewal")
	}
	activity := manager.View().Traces[receipt.TraceID].Activity
	if activity.Settled != 500*time.Millisecond || activity.Reserved != 0 || activity.Revision != 2 {
		t.Fatalf("settlement=%+v", activity)
	}
}
