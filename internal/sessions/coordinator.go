package sessions

import (
	"context"
	"errors"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

type commandResult struct {
	value any
	err   error
}
type command struct {
	ctx   context.Context
	fn    func(*runtime) (any, error)
	reply chan commandResult
}
type execution struct {
	scope                 agent.ExecutionScope
	ctx                   context.Context
	cancel                context.CancelFunc
	done                  chan struct{}
	budget                *agent.BudgetLedger
	activity              *activityLease
	turnID                string // owned by the session mailbox
	turnSelectionRevision uint64 // fixed when PrepareNextTurn selects this turn
	loop                  *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage]
	pauseID               string
	checkpoint            *checkpointResult
	input                 agent.InputRef
	resume                *state.CheckpointRef
	resumeID              string
	toolChunks            map[string]toolChunkPosition // mailbox-owned, one execution segment only
}
type toolChunkPosition struct {
	streamID string
	seq      uint64
}
type runtime struct {
	clock       activityClock
	opts        Options
	manager     *state.Manager
	mailbox     chan command
	done        chan struct{}
	subs        map[int]*subscription
	nextSub     int
	closing     bool
	closeErr    error // read only after done is closed
	generation  string
	active      *execution
	cursor      uint64
	modelChunks map[string]uint64 // mailbox-owned temporary stream positions
}

func (rt *runtime) writable() error {
	if rt.closing {
		return product.NewError(product.CodeStateConflict, "session is closing")
	}
	if rt.opts.ReadOnly {
		return product.NewError(product.CodePermissionDenied, "session is read-only")
	}
	if err := rt.manager.Fault(); err != nil {
		return err
	}
	if rt.manager.View().RepairRequired {
		return product.NewError(product.CodeStorageUnavailable, "journal requires repair")
	}
	return nil
}
func (s *AgentSession) Cancel(ctx context.Context, traceID string) error {
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		tr := rt.manager.View().Traces[traceID]
		if tr == nil {
			return nil, product.NewError(product.CodeNotFound, "trace not found")
		}
		if terminal(tr.State) {
			return nil, nil
		}
		if rt.active == nil || rt.active.scope.TraceID != traceID {
			if tr.Started {
				return nil, rt.cancelInterrupted(ctx, tr)
			}
			return nil, rt.manager.SetTraceState(ctx, traceID, "cancelled", false)
		}
		if err := rt.manager.SetTraceState(ctx, traceID, "cancelling", false); err != nil {
			return nil, err
		}
		rt.active.cancel()
		return rt.active.done, nil
	})
	if err != nil {
		return err
	}
	if value == nil {
		return nil
	}
	select {
	case <-value.(chan struct{}):
		return s.rt.manager.Fault()
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *AgentSession) ContinueQueue(ctx context.Context, traceID string) error {
	_, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		tr := rt.manager.View().Traces[traceID]
		if tr == nil || tr.State != "queued" {
			return nil, product.NewError(product.CodeStateConflict, "only queued trace can continue")
		}
		if tr.Generation != rt.generation {
			return nil, product.NewError(product.CodeIncompatibleVersion, "queued generation cannot be replaced")
		}
		view := rt.manager.View()
		if view.HasUnresolvedEffects() {
			return nil, product.NewError(product.CodeReconciliationRequired, "unresolved tool effects block queued work")
		}
		if active := view.Traces[view.ActiveTrace]; active != nil && active.State == "paused" {
			return nil, product.NewError(product.CodeReconciliationRequired, "interrupted execution blocks queued work")
		}
		if tr.Hold {
			if err := rt.manager.ReleaseHold(ctx, traceID); err != nil {
				return nil, err
			}
		}
		rt.schedule()
		return nil, nil
	})
	return err
}
func (s *AgentSession) Close(ctx context.Context) error {
	select {
	case <-s.rt.done:
		return s.rt.closeErr
	default:
	}
	_, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		rt.closing = true
		if rt.active != nil {
			rt.active.cancel()
		}
		return nil, nil
	})
	if err != nil {
		select {
		case <-s.rt.done:
			return s.rt.closeErr
		default:
			return err
		}
	}
	select {
	case <-s.rt.done:
		return s.rt.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (rt *runtime) schedule() {
	if rt.closing || rt.active != nil || rt.opts.ReadOnly || rt.manager.Fault() != nil {
		return
	}
	v := rt.manager.View()
	if v.ActiveTrace != "" || v.HasUnresolvedEffects() {
		return
	}
	for _, id := range v.Independent {
		in := v.Inputs[id]
		tr := v.Traces[in.TraceID]
		if in.State != "pending" || tr.State != "queued" || tr.Hold {
			continue
		}
		if tr.Generation != rt.generation {
			return
		}
		if err := rt.manager.SetTraceState(context.Background(), tr.ID, "running", false); err != nil {
			return
		}
		tr = rt.manager.View().Traces[tr.ID]
		ctx, cancel := context.WithCancel(context.Background())
		frame := &execution{scope: agent.ExecutionScope{SessionID: rt.opts.SessionID, BranchID: v.BranchID, TraceID: tr.ID, InvocationID: tr.InvocationID, ExecutionID: agent.MustID(), Generation: tr.Generation}, ctx: ctx, cancel: cancel, done: make(chan struct{}), budget: agent.NewBudget(tr.Limits)}
		frame.budget.Restore(tr.Usage)
		rt.setBudgetPersistence(frame)
		rt.active = frame
		go rt.runSegment(frame, in.ID)
		return
	}
}
func (rt *runtime) setBudgetPersistence(frame *execution) {
	// Only configure a fresh, unpublished ledger. The callback's frame and
	// ExecutionID stay immutable even when the trace starts another segment.
	frame.budget.SetPersist(func(usage agent.Usage) error {
		return rt.do(context.Background(), func(rt *runtime) error {
			if !rt.matchesExecution(frame.scope) {
				return product.NewError(product.CodeStateConflict, "budget execution is no longer active")
			}
			if err := frame.ctx.Err(); err != nil {
				return err
			}
			if frame.activity == nil {
				return activityExhausted()
			}
			if err := frame.activity.allowed(); err != nil {
				return err
			}
			return rt.manager.SaveTraceBudget(context.Background(), frame.scope.TraceID, usage)
		})
	})
}

func (rt *runtime) segmentFinished(frame *execution, runErr error) {
	if rt.active != frame {
		return
	}
	v := rt.manager.View()
	tr := v.Traces[frame.scope.TraceID]
	if tr == nil {
		return
	}
	if runErr == nil && frame.ctx.Err() != nil {
		runErr = frame.ctx.Err()
	}
	if frame.resumeID != "" && rt.manager.Fault() == nil {
		next := "completed"
		if tr.State == "cancelling" || rt.closing {
			next = "cancelled"
		} else if runErr != nil {
			next = "failed"
		}
		if err := rt.manager.TransitionOperation(context.Background(), frame.resumeID, 1, next, frame.scope.ExecutionID, ""); err != nil {
			runErr = errors.Join(runErr, err)
		}
		v = rt.manager.View()
		tr = v.Traces[frame.scope.TraceID]
	}
	if frame.pauseID != "" && !rt.closing && frame.ctx.Err() == nil && tr.State == "running" && runErr == nil && frame.checkpoint != nil && frame.checkpoint.valid && !v.TraceHasUnresolvedEffects(tr.ID) {
		cp, err := rt.pauseReference(frame, frame.input, frame.checkpoint.ref, v)
		if err != nil {
			runErr = err
		}
		if runErr == nil {
			if err := rt.manager.CommitPause(context.Background(), tr.ID, frame.pauseID, cp); err == nil {
				frame.cancel()
				frame.toolChunks = nil
				frame.loop = nil
				rt.active = nil
				close(frame.done)
				return
			} else {
				runErr = err
			}
		}
	}
	if frame.pauseID != "" && rt.manager.Fault() == nil {
		_ = rt.manager.TransitionOperation(context.Background(), frame.pauseID, 1, "failed", "", "pause checkpoint unavailable")
	}
	if runErr == nil && v.HasUnresolvedEffects() {
		runErr = product.NewError(product.CodeReconciliationRequired, "execution stopped with unresolved tool effects")
	}
	if !rt.closing && tr.State == "running" && runErr == nil && rt.manager.Fault() == nil {
	continuation:
		for _, queue := range [][]string{v.Steering, v.Follow} {
			for _, id := range queue {
				in := v.Inputs[id]
				if in.TraceID == tr.ID && in.State == "pending" {
					used, limits := tr.Usage, tr.Limits
					if used.LogicalModelCalls >= limits.TraceLogicalModelCalls || used.TransportRequests >= limits.TraceTransportRequests {
						runErr = product.NewError(product.CodeBudgetExhausted, "model budget exhausted")
						break continuation
					}
					frame.cancel()
					ctx, cancel := context.WithCancel(context.Background())
					next := &execution{scope: frame.scope, ctx: ctx, cancel: cancel, done: frame.done, budget: agent.NewBudget(limits)}
					next.scope.ExecutionID = agent.MustID()
					next.budget.Restore(used)
					rt.setBudgetPersistence(next)
					rt.active = next
					go rt.runSegment(next, id)
					return
				}
			}
		}
	}
	state := "completed"
	switch {
	case tr.State == "cancelling":
		state = "cancelled"
	case rt.closing:
		state = "paused"
	case runErr != nil:
		state = "failed"
	}
	if err := rt.manager.ConfirmExecutionStopped(context.Background(), tr.ID); err != nil {
		runErr = errors.Join(runErr, err)
		if state == "completed" {
			state = "failed"
		}
	}
	if frame.pauseID == "" || tr.State == "cancelling" {
		if err := rt.finishInterruptedTurn(frame); err != nil && state == "completed" {
			state = "failed"
		}
	}
	if rt.manager.Fault() == nil {
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			_ = rt.manager.SaveTraceError(context.Background(), tr.ID, runErr.Error())
		}
		_ = rt.manager.SetTraceState(context.Background(), tr.ID, state, terminal(state))
	}
	frame.cancel()
	frame.toolChunks = nil
	rt.active = nil
	close(frame.done)
	if terminal(state) {
		rt.schedule()
	}
}
func (rt *runtime) loop() {
	defer close(rt.done)
	for {
		cmd := <-rt.mailbox
		var result commandResult
		if err := cmd.ctx.Err(); err != nil {
			result.err = err
		} else {
			result.value, result.err = cmd.fn(rt)
		}
		rt.publishCommitted()
		cmd.reply <- result
		if rt.closing && rt.active == nil {
			for _, sub := range rt.subs {
				sub.close(nil)
				<-sub.stopped
			}
			rt.closeErr = rt.opts.Store.Close()
			return
		}
	}
}
func (rt *runtime) call(ctx context.Context, fn func(*runtime) (any, error)) (any, error) {
	cmd := command{ctx: ctx, fn: fn, reply: make(chan commandResult, 1)}
	select {
	case <-rt.done:
		return nil, product.NewError(product.CodeStateConflict, "session is closed")
	default:
	}
	select {
	case rt.mailbox <- cmd:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-rt.done:
		return nil, product.NewError(product.CodeStateConflict, "session is closed")
	}
	select {
	case r := <-cmd.reply:
		return r.value, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-rt.done:
		select {
		case r := <-cmd.reply:
			return r.value, r.err
		default:
			return nil, product.NewError(product.CodeStateConflict, "session is closed")
		}
	}
}
func (rt *runtime) do(ctx context.Context, fn func(*runtime) error) error {
	_, err := rt.call(ctx, func(rt *runtime) (any, error) { return nil, fn(rt) })
	return err
}

func terminal(state string) bool {
	return state == "completed" || state == "cancelled" || state == "failed"
}
