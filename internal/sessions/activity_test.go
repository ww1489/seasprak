package sessions

import (
	"context"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type manualActivityClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualActivityTimer
}
type manualActivityTimer struct {
	clock   *manualActivityClock
	at      time.Time
	fn      func()
	stopped bool
	fired   bool
}

func newManualActivityClock() *manualActivityClock { return &manualActivityClock{now: time.Unix(1, 0)} }
func (c *manualActivityClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *manualActivityClock) AfterFunc(d time.Duration, fn func()) activityTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualActivityTimer{clock: c, at: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, t)
	return t
}
func (t *manualActivityTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped && !t.fired
	t.stopped = true
	return was
}
func (c *manualActivityClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var callbacks []func()
	for _, t := range c.timers {
		if !t.stopped && !t.fired && !t.at.After(c.now) {
			t.fired = true
			callbacks = append(callbacks, t.fn)
		}
	}
	c.mu.Unlock()
	for _, fn := range callbacks {
		fn()
	}
}

type activityModel struct {
	calls   atomic.Int32
	started chan context.Context
	release chan struct{}
	inner   *testkit.FakeModel
}

func (m *activityModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	m.calls.Add(1)
	m.started <- ctx
	<-m.release
	return m.inner.Generate(ctx, in, opts...)
}
func (m *activityModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}
func activitySession(t *testing.T, limit time.Duration, wrapped func(store.Store) store.Store) (*AgentSession, *state.Manager, *manualActivityClock, *activityModel) {
	t.Helper()
	st, err := memory.Open("activity", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	var backend store.Store = st
	if wrapped != nil {
		backend = wrapped(st)
	}
	manager, err := state.NewManager(backend, "activity")
	if err != nil {
		t.Fatal(err)
	}
	model := &activityModel{started: make(chan context.Context, 10), release: make(chan struct{}, 10), inner: testkit.NewFake(testkit.Step{Text: "done", Repeat: true})}
	s, err := Start(Options{SessionID: "activity", Profile: ProfileMemory, Store: backend, Model: model, Limits: config.Limits{ActivityBudget: limit}}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	clock := newManualActivityClock()
	if err := s.rt.do(context.Background(), func(rt *runtime) error { rt.clock = clock; return nil }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case model.release <- struct{}{}:
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s, manager, clock, model
}
func activitySubmit(t *testing.T, s *AgentSession) agent.InputReceipt {
	t.Helper()
	r, err := s.SubmitInput(context.Background(), agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"hi"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func activityStarted(t *testing.T, m *activityModel) context.Context {
	t.Helper()
	select {
	case ctx := <-m.started:
		return ctx
	case <-time.After(time.Second):
		t.Fatal("model did not start")
		return nil
	}
}
func activityFrame(t *testing.T, s *AgentSession) *execution {
	t.Helper()
	v, err := s.rt.call(context.Background(), func(rt *runtime) (any, error) { return rt.active, nil })
	if err != nil {
		t.Fatal(err)
	}
	return v.(*execution)
}
func activityWait(t *testing.T, f *execution) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(time.Second):
		t.Fatal("execution did not exit")
	}
}

func TestActivitySafeExit250msAndIdleHours(t *testing.T) {
	s, m, clock, model := activitySession(t, 3*time.Second, nil)
	r := activitySubmit(t, s)
	activityStarted(t, model)
	frame := activityFrame(t, s)
	before := m.View().LastSeq
	if a := m.View().Traces[r.TraceID].Activity; a.Reserved != time.Second {
		t.Fatalf("missing committed reservation: %+v", a)
	}
	clock.advance(250 * time.Millisecond)
	model.release <- struct{}{}
	activityWait(t, frame)
	tr := m.View().Traces[r.TraceID]
	if tr.Activity.Settled != 250*time.Millisecond || tr.Activity.Reserved != 0 || tr.Activity.Uncertain != 0 || !tr.ExecutionStopped || model.calls.Load() != 1 || m.View().LastSeq <= before {
		t.Fatalf("trace=%+v calls=%d", tr, model.calls.Load())
	}
	seq := m.View().LastSeq
	clock.advance(8 * time.Hour)
	if m.View().Traces[r.TraceID].Activity != tr.Activity || m.View().LastSeq != seq || model.calls.Load() != 1 {
		t.Fatal("idle wall clock was charged or executed")
	}
}
