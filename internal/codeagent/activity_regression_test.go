package codeagent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
	"sync/atomic"
	"testing"
	"time"
)

type activityStore struct {
	store.Store
	hook      func(state.ActivityBudget) error
	last      uint64
	committed chan state.ActivityBudget
}

func (s *activityStore) Append(ctx context.Context, id string, e store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	var reservation *state.ActivityBudget
	for _, r := range c.ControlRecords {
		if r.Type == "trace" {
			var tr state.TraceState
			if err := json.Unmarshal(r.Payload, &tr); err != nil {
				return store.CommitReceipt{}, err
			}
			if tr.Activity.Reserved > 0 && tr.Activity.Revision > s.last {
				a := tr.Activity
				reservation = &a
				break
			}
		}
	}
	if reservation != nil && s.hook != nil {
		if err := s.hook(*reservation); err != nil {
			return store.CommitReceipt{}, err
		}
	}
	receipt, err := s.Store.Append(ctx, id, e, c)
	if err == nil && reservation != nil {
		s.last = reservation.Revision
		if s.committed != nil {
			s.committed <- *reservation
		}
	}
	return receipt, err
}
func awaitActivitySignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("activity signal missing")
	}
}

func TestActivityGateRejectsRequestBeforeDelayedTimerRuns(t *testing.T) {
	s, m, clock, model := activitySession(t, time.Second, nil)
	r := activitySubmit(t, s)
	ctx := activityStarted(t, model)
	frame := activityFrame(t, s)
	before := m.View().LastSeq
	clock.mu.Lock()
	clock.now = clock.now.Add(time.Second)
	clock.mu.Unlock()
	if ctx.Err() != nil {
		t.Fatal("test watchdog fired too early")
	}
	pe, ok := product.AsError(frame.budget.OccupyModel())
	if !ok || pe.Code != product.CodeBudgetExhausted || model.calls.Load() != 1 || frame.budget.Snapshot().TransportRequests != 1 || m.View().LastSeq != before {
		t.Fatal("expired lease allowed a new model request")
	}
	clock.advance(0)
	awaitActivitySignal(t, ctx.Done())
	model.release <- struct{}{}
	activityWait(t, frame)
	if tr := m.View().Traces[r.TraceID]; tr.Activity.Settled != time.Second || !tr.ExecutionStopped {
		t.Fatalf("trace=%+v", tr)
	}
}

func TestActivitySuccessfulRenewalPreservesRequestCounts(t *testing.T) {
	var backend *activityStore
	s, m, clock, model := activitySession(t, 3*time.Second, func(st store.Store) store.Store {
		backend = &activityStore{Store: st, committed: make(chan state.ActivityBudget, 10)}
		return backend
	})
	r := activitySubmit(t, s)
	ctx := activityStarted(t, model)
	frame := activityFrame(t, s)
	<-backend.committed
	clock.advance(500 * time.Millisecond)
	select {
	case <-backend.committed:
	case <-time.After(time.Second):
		t.Fatal("renewal missing")
	}
	// The receipt channel is sent inside Append; a mailbox barrier also waits
	// for the successful lease installation before advancing the fake clock.
	if err := s.rt.do(context.Background(), func(*runtime) error { return nil }); err != nil {
		t.Fatal(err)
	}
	tr := m.View().Traces[r.TraceID]
	if tr.Activity.Settled != 500*time.Millisecond || tr.Activity.Reserved != time.Second || tr.Activity.Revision != 2 || tr.Usage.TransportRequests != 1 || ctx.Err() != nil {
		t.Fatalf("trace=%+v", tr)
	}
	clock.advance(250 * time.Millisecond)
	model.release <- struct{}{}
	activityWait(t, frame)
	tr = m.View().Traces[r.TraceID]
	if tr.Activity.Settled != 750*time.Millisecond || tr.Activity.Reserved != 0 || tr.Activity.Revision != 3 || tr.Usage.TransportRequests != 1 || model.calls.Load() != 1 {
		t.Fatalf("trace=%+v", tr)
	}
}

func TestActivityInitialReservationFailureStartsNoModel(t *testing.T) {
	var session *AgentSession
	frames := make(chan *execution, 1)
	s, m, _, model := activitySession(t, time.Second, func(st store.Store) store.Store {
		return &activityStore{Store: st, hook: func(a state.ActivityBudget) error {
			frames <- session.rt.active
			return errors.New("initial reservation failed")
		}}
	})
	session = s
	r := activitySubmit(t, s)
	var frame *execution
	select {
	case frame = <-frames:
	case <-time.After(time.Second):
		t.Fatal("reservation did not reach append")
	}
	activityWait(t, frame)
	tr := m.View().Traces[r.TraceID]
	if model.calls.Load() != 0 || tr.Activity.Reserved != 0 || tr.ExecutionStopped || m.View().LastSeq != r.AcceptedCommit+1 {
		t.Fatalf("trace=%+v calls=%d seq=%d", tr, model.calls.Load(), m.View().LastSeq)
	}
}

func TestActivityCommitLatencyConsumesGrantedTime(t *testing.T) {
	var clock *manualActivityClock
	s, m, c, model := activitySession(t, time.Second, func(st store.Store) store.Store {
		return &activityStore{Store: st, hook: func(a state.ActivityBudget) error {
			if a.Revision == 1 {
				clock.advance(200 * time.Millisecond)
			}
			return nil
		}}
	})
	clock = c
	r := activitySubmit(t, s)
	activityStarted(t, model)
	frame := activityFrame(t, s)
	clock.advance(50 * time.Millisecond)
	model.release <- struct{}{}
	activityWait(t, frame)
	tr := m.View().Traces[r.TraceID]
	if tr.Activity.Settled != 250*time.Millisecond || tr.Activity.Reserved != 0 || tr.Activity.Revision != 2 || model.calls.Load() != 1 {
		t.Fatalf("trace=%+v calls=%d", tr, model.calls.Load())
	}
}

func TestActivityBlockedRenewalExpiresOutsideMailbox(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var backend *activityStore
	s, m, clock, model := activitySession(t, 3*time.Second, func(st store.Store) store.Store {
		backend = &activityStore{Store: st, committed: make(chan state.ActivityBudget, 10), hook: func(a state.ActivityBudget) error {
			if a.Revision == 2 {
				close(entered)
				<-release
			}
			return nil
		}}
		return backend
	})
	r := activitySubmit(t, s)
	modelCtx := activityStarted(t, model)
	frame := activityFrame(t, s)
	<-backend.committed
	clock.advance(500 * time.Millisecond)
	awaitActivitySignal(t, entered)
	clock.advance(500 * time.Millisecond)
	awaitActivitySignal(t, modelCtx.Done())
	// Manager.View would wait for Append. Inspect the already committed store
	// prefix instead, without unblocking persistence or the uncooperative model.
	journal, err := backend.Store.Load(context.Background(), "activity")
	if err != nil {
		t.Fatal(err)
	}
	var persisted state.TraceState
	for _, c := range journal.Commits {
		for _, r := range c.ControlRecords {
			if r.Type == "trace" {
				_ = json.Unmarshal(r.Payload, &persisted)
			}
		}
	}
	if persisted.ExecutionStopped || persisted.Activity.Revision != 1 || model.calls.Load() != 1 {
		t.Fatalf("premature stop: %+v calls=%d", persisted, model.calls.Load())
	}
	select {
	case <-frame.done:
		t.Fatal("timer pretended the model exited")
	default:
	}
	close(release)
	select {
	case <-backend.committed:
	case <-time.After(time.Second):
		t.Fatal("late commit missing")
	}
	if modelCtx.Err() == nil || model.calls.Load() != 1 {
		t.Fatal("late renewal revived execution")
	}
	model.release <- struct{}{}
	activityWait(t, frame)
	tr := m.View().Traces[r.TraceID]
	if !tr.ExecutionStopped || tr.State != "failed" || tr.Activity.Settled != time.Second || tr.Activity.Reserved != 0 || model.calls.Load() != 1 {
		t.Fatalf("trace=%+v calls=%d", tr, model.calls.Load())
	}
}

func TestActivityRenewalCommitChecksOldDeadlineWithoutTimerDelivery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		at      time.Duration
		expired bool
	}{
		{name: "just_before_deadline", at: time.Second - time.Nanosecond},
		{name: "exact_deadline", at: time.Second, expired: true},
		{name: "after_deadline", at: 1100 * time.Millisecond, expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var backend *activityStore
			s, m, clock, model := activitySession(t, 3*time.Second, func(st store.Store) store.Store {
				backend = &activityStore{Store: st, committed: make(chan state.ActivityBudget, 10), hook: func(a state.ActivityBudget) error {
					if a.Revision == 2 {
						close(entered)
						<-release
					}
					return nil
				}}
				return backend
			})
			// Unblock persistence before session cleanup even if an assertion fails.
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			r := activitySubmit(t, s)
			ctx := activityStarted(t, model)
			frame := activityFrame(t, s)
			initial := <-backend.committed
			if initial.Reserved != time.Second {
				t.Fatalf("initial reservation=%+v", initial)
			}
			clock.advance(500 * time.Millisecond)
			awaitActivitySignal(t, entered)
			// Move time without dispatching the old watchdog. Append must check
			// its old deadline itself before installing the replacement lease.
			clock.mu.Lock()
			clock.now = clock.now.Add(tc.at - 500*time.Millisecond)
			clock.mu.Unlock()
			if ctx.Err() != nil {
				t.Fatal("watchdog unexpectedly delivered during blocked append")
			}
			close(release)
			if err := s.rt.do(context.Background(), func(*runtime) error { return nil }); err != nil {
				t.Fatal(err)
			}
			tr := m.View().Traces[r.TraceID]
			if tr.ExecutionStopped || tr.Activity.Revision != 2 || tr.Activity.Settled != 500*time.Millisecond || tr.Activity.Reserved != time.Second || tr.Limits.ActivityBudget != 3*time.Second {
				t.Fatalf("renewal lost committed facts or claimed exit: %+v", tr)
			}
			select {
			case <-frame.done:
				t.Fatal("renewal pretended the blocked model exited")
			default:
			}
			seq := m.View().LastSeq
			if tc.expired {
				if ctx.Err() == nil {
					t.Error("late renewal revived execution while the old watchdog was undelivered")
				}
				pe, ok := product.AsError(frame.activity.allowed())
				if !ok || pe.Code != product.CodeBudgetExhausted {
					t.Errorf("late renewal lost budget exhaustion: error=%v", pe)
				}
				// Budget persistence checks the cancelled execution context first.
				if err := frame.budget.OccupyModel(); !errors.Is(err, context.Canceled) {
					t.Errorf("late renewal admitted a new request: error=%v", err)
				}
			} else if err := frame.activity.allowed(); err != nil || ctx.Err() != nil {
				t.Errorf("on-time renewal rejected: gate=%v context=%v", err, ctx.Err())
			}
			if model.calls.Load() != 1 || frame.budget.Snapshot().TransportRequests != 1 || tr.Usage.TransportRequests != 1 || m.View().LastSeq != seq {
				t.Error("renewal or rejected request increased model calls, request usage, or commits")
			}
			model.release <- struct{}{}
			activityWait(t, frame)
			tr = m.View().Traces[r.TraceID]
			wantState := "completed"
			if tc.expired {
				wantState = "failed"
			}
			if !tr.ExecutionStopped || tr.State != wantState || tr.Activity.Revision != 3 || tr.Activity.Settled != tc.at || tr.Activity.Reserved != 0 || model.calls.Load() != 1 {
				t.Fatalf("real exit did not settle committed renewal: %+v calls=%d", tr, model.calls.Load())
			}
		})
	}
}

func TestActivityRenewalFailureKeepsOccupancyUntilRealExit(t *testing.T) {
	var backend *activityStore
	s, m, clock, model := activitySession(t, 3*time.Second, func(st store.Store) store.Store {
		backend = &activityStore{Store: st, hook: func(a state.ActivityBudget) error {
			if a.Revision == 2 {
				return errors.New("injected renewal failure")
			}
			return nil
		}}
		return backend
	})
	r := activitySubmit(t, s)
	modelCtx := activityStarted(t, model)
	frame := activityFrame(t, s)
	before := m.View().LastSeq
	clock.advance(500 * time.Millisecond)
	awaitActivitySignal(t, modelCtx.Done())
	tr := m.View().Traces[r.TraceID]
	if tr.ExecutionStopped || tr.Activity.Reserved != time.Second || m.View().LastSeq != before || model.calls.Load() != 1 {
		t.Fatalf("failure changed facts: %+v", tr)
	}
	select {
	case <-frame.done:
		t.Fatal("cancel released active execution")
	default:
	}
	model.release <- struct{}{}
	activityWait(t, frame)
	// A failed store cannot persist even the actual exit; reopen keeps the
	// reservation conservative rather than refunding it or inventing proof.
	reopened, err := state.NewManager(backend.Store, "activity")
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.View().Traces[r.TraceID]
	if got.ExecutionStopped || got.Activity.Uncertain != time.Second || got.Activity.Reserved != 0 || model.calls.Load() != 1 {
		t.Fatalf("reopened=%+v", got)
	}
}

func TestActivityRemaining300msExpiryAndNoFalseStop(t *testing.T) {
	s, m, clock, model := activitySession(t, 300*time.Millisecond, nil)
	r := activitySubmit(t, s)
	ctx := activityStarted(t, model)
	frame := activityFrame(t, s)
	if a := m.View().Traces[r.TraceID].Activity; a.Reserved != 300*time.Millisecond {
		t.Fatal(a)
	}
	before := m.View().LastSeq
	clock.advance(300 * time.Millisecond)
	awaitActivitySignal(t, ctx.Done())
	if model.calls.Load() != 1 || m.View().Traces[r.TraceID].ExecutionStopped || m.View().LastSeq != before {
		t.Fatal("expiry manufactured a commit or exit")
	}
	model.release <- struct{}{}
	activityWait(t, frame)
	tr := m.View().Traces[r.TraceID]
	if tr.Activity.Settled != 300*time.Millisecond || tr.State != "failed" || !tr.ExecutionStopped || model.calls.Load() != 1 {
		t.Fatalf("trace=%+v", tr)
	}
}

func TestActivityOldTimerCannotCancelNextExecution(t *testing.T) {
	s, m, clock, model := activitySession(t, 3*time.Second, nil)
	r := activitySubmit(t, s)
	activityStarted(t, model)
	old := activityFrame(t, s)
	clock.mu.Lock()
	callbacks := append([]*manualActivityTimer(nil), clock.timers...)
	clock.mu.Unlock()
	_, err := s.SubmitInput(context.Background(), agent.InputCommand{Kind: "follow_up", TargetTraceID: r.TraceID, Content: []byte(`{"text":"next"}`)})
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(250 * time.Millisecond)
	model.release <- struct{}{}
	nextCtx := activityStarted(t, model)
	next := activityFrame(t, s)
	if old.scope.ExecutionID == next.scope.ExecutionID {
		t.Fatal("execution identity reused")
	}
	for _, timer := range callbacks {
		timer.fn()
	} // Deliberately deliver stopped callbacks.
	if nextCtx.Err() != nil || model.calls.Load() != 2 {
		t.Fatal("old timer affected new segment")
	}
	clock.advance(250 * time.Millisecond)
	model.release <- struct{}{}
	activityWait(t, next)
	tr := m.View().Traces[r.TraceID]
	if tr.Activity.Settled != 500*time.Millisecond || tr.Activity.Reserved != 0 || tr.State != "completed" || model.calls.Load() != 2 {
		t.Fatalf("trace=%+v", tr)
	}
}

func TestActivityPausedHoursDoNotIncreaseUsage(t *testing.T) {
	s, m, clock, model := activitySession(t, 3*time.Second, nil)
	r := activitySubmit(t, s)
	modelCtx := activityStarted(t, model)
	clock.advance(250 * time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- s.Close(context.Background()) }()
	awaitActivitySignal(t, modelCtx.Done())
	model.release <- struct{}{}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	tr := m.View().Traces[r.TraceID]
	seq := m.View().LastSeq
	if tr.State != "paused" || tr.Activity.Settled != 250*time.Millisecond || tr.Activity.Reserved != 0 || !tr.ExecutionStopped {
		t.Fatalf("trace=%+v", tr)
	}
	clock.advance(8 * time.Hour)
	if m.View().LastSeq != seq || m.View().Traces[r.TraceID].Activity != tr.Activity || model.calls.Load() != 1 {
		t.Fatal("pause billed wall clock or ran model")
	}
}

func TestActivityConcurrentToolsUseOneTraceClock(t *testing.T) {
	s, m, clock, model := activitySession(t, 3*time.Second, nil)
	started, release := make(chan struct{}, 2), make(chan struct{}, 2)
	var calls atomic.Int32
	model.inner = testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "one", Name: "work", Arguments: `{"n":1}`}, {CallID: "two", Name: "work", Arguments: `{"n":2}`}}}, testkit.Step{Text: "done"})
	err := s.rt.do(context.Background(), func(rt *runtime) error {
		rt.opts.Tools = []tools.Definition{{Name: "work", Schema: []byte(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", Resources: []agent.ExecutionResource{{Identity: "activity-shared"}}}, Run: func(context.Context, json.RawMessage) (string, error) {
			calls.Add(1)
			started <- struct{}{}
			<-release
			return "ok", nil
		}}}
		rt.opts.ToolInfos = []*schema.ToolInfo{testkit.ToolInfo("work", "")}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r := activitySubmit(t, s)
	activityStarted(t, model)
	frame := activityFrame(t, s)
	model.release <- struct{}{}
	awaitActivitySignal(t, started)
	awaitActivitySignal(t, started)
	clock.advance(250 * time.Millisecond)
	release <- struct{}{}
	release <- struct{}{}
	activityStarted(t, model)
	model.release <- struct{}{}
	activityWait(t, frame)
	tr := m.View().Traces[r.TraceID]
	if tr.Activity.Settled != 250*time.Millisecond || tr.Activity.Revision != 2 || tr.Usage.ToolExecutions != 2 || calls.Load() != 2 || model.calls.Load() != 2 {
		t.Fatalf("trace=%+v tools=%d models=%d", tr, calls.Load(), model.calls.Load())
	}
}
