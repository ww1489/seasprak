package session

import (
	"context"
	"errors"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/agent"
	"github.com/ww1489/seasprak/agent/tools"
	"github.com/ww1489/seasprak/model"
	"github.com/ww1489/seasprak/session/history"
)

const (
	ProfileDefault = ""
	ProfileMemory  = "memory"
)

type Options struct {
	Workspace             string
	StateRoot             string
	SessionID             string
	Model                 einomodel.AgenticModel
	Tools                 []tools.Definition
	ToolInfos             []*schema.ToolInfo
	Limits                model.Limits
	Profile               string
	Instruction           string
	Store                 history.SessionStore
	Principal             string
	ReadOnly              bool
	GenerationFingerprint string
}
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
	scope  agent.ExecutionScope
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	budget *agent.BudgetLedger
	turnID string // owned by the session mailbox
}
type runtime struct {
	opts       Options
	manager    *history.Manager
	mailbox    chan command
	done       chan struct{}
	subs       map[int]*subscription
	nextSub    int
	closing    bool
	closeErr   error // read only after done is closed
	generation string
	active     *execution
	cursor     uint64
}
type AgentSession struct{ rt *runtime }

func Start(opts Options, manager *history.Manager, generation string) (*AgentSession, error) {
	if !opts.ReadOnly {
		if opts.Profile == ProfileDefault {
			return nil, model.NewError(model.CodeResourceUnavailable, "default file and process backends are not available")
		}
		if opts.Profile != ProfileMemory {
			return nil, model.NewError(model.CodeUnsupportedCapability, "unknown capability profile")
		}
		if opts.Model == nil {
			return nil, model.NewError(model.CodeInvalidArgument, "model is required")
		}
	}
	opts.Limits = opts.Limits.WithDefaults()
	opts.Tools = append([]tools.Definition(nil), opts.Tools...)
	for i := range opts.Tools {
		opts.Tools[i].Schema = append([]byte(nil), opts.Tools[i].Schema...)
	}
	if opts.Store == nil || manager == nil {
		return nil, model.NewError(model.CodeInvalidArgument, "session store and manager are required")
	}
	rt := &runtime{opts: opts, manager: manager, mailbox: make(chan command, 64), done: make(chan struct{}), subs: map[int]*subscription{}, generation: generation}
	view := manager.View()
	// Opening never replays an execution or silently releases an old queue.
	if !opts.ReadOnly && !view.RepairRequired {
		for id, tr := range view.Traces {
			if tr.Started && !terminal(tr.State) && tr.State != "paused" {
				if err := manager.SetTraceState(context.Background(), id, "paused", false); err != nil {
					return nil, err
				}
			}
			if tr.State == "queued" && !tr.Hold {
				if err := manager.HoldIndependent(context.Background(), id); err != nil {
					return nil, err
				}
			}
		}
	}
	rt.cursor = manager.View().Cursor
	go rt.loop()
	return &AgentSession{rt: rt}, nil
}
func (rt *runtime) writable() error {
	if rt.closing {
		return model.NewError(model.CodeStateConflict, "session is closing")
	}
	if rt.opts.ReadOnly {
		return model.NewError(model.CodePermissionDenied, "session is read-only")
	}
	if err := rt.manager.Fault(); err != nil {
		return err
	}
	if rt.manager.View().RepairRequired {
		return model.NewError(model.CodeStorageUnavailable, "journal requires repair")
	}
	return nil
}
func (s *AgentSession) SubmitInput(ctx context.Context, cmd agent.InputCommand) (agent.InputReceipt, error) {
	cmd.Content = append([]byte(nil), cmd.Content...)
	if cmd.Principal == "" {
		cmd.Principal = s.rt.opts.Principal
	}
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		target := agent.TargetAgent{Name: "main", Version: "main-v1", Generation: rt.generation}
		if cmd.TargetAgent != "" && (cmd.Kind == "prompt" || cmd.Kind == "chat") {
			target.Name = cmd.TargetAgent
		}
		before := rt.manager.View().LastSeq
		receipt, err := rt.manager.AcceptWithLimits(ctx, cmd, target, rt.opts.Limits)
		if err != nil {
			return nil, err
		}
		// A replayed acceptance receipt never schedules work again.
		if receipt.AcceptedCommit > before {
			rt.schedule()
		}
		return receipt, nil
	})
	if err != nil {
		return agent.InputReceipt{}, err
	}
	return value.(agent.InputReceipt), nil
}
func (s *AgentSession) Cancel(ctx context.Context, traceID string) error {
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		tr := rt.manager.View().Traces[traceID]
		if tr == nil {
			return nil, model.NewError(model.CodeNotFound, "trace not found")
		}
		if terminal(tr.State) {
			return nil, nil
		}
		if rt.active == nil || rt.active.scope.TraceID != traceID {
			if tr.Started {
				return nil, model.NewError(model.CodeReconciliationRequired, "interrupted execution needs reconciliation")
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
			return nil, model.NewError(model.CodeStateConflict, "only queued trace can continue")
		}
		if tr.Generation != rt.generation {
			return nil, model.NewError(model.CodeIncompatibleVersion, "queued generation cannot be replaced")
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
func (s *AgentSession) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	v := s.rt.manager.View()
	return Snapshot{SessionID: s.rt.opts.SessionID, Cursor: v.Cursor, ActiveTrace: v.ActiveTrace, Traces: v.Traces, Inputs: v.Inputs, Messages: v.Messages, Turns: v.Turns, Calls: v.Calls, RepairRequired: v.RepairRequired}, nil
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
	if v.ActiveTrace != "" {
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
		ctx, cancel := context.WithTimeout(context.Background(), tr.Limits.ActivityBudget)
		frame := &execution{scope: agent.ExecutionScope{SessionID: rt.opts.SessionID, BranchID: v.BranchID, TraceID: tr.ID, InvocationID: tr.InvocationID, ExecutionID: agent.MustID(), Generation: tr.Generation}, ctx: ctx, cancel: cancel, done: make(chan struct{}), budget: agent.NewBudget(tr.Limits)}
		frame.budget.Restore(tr.Usage)
		frame.budget.SetPersist(func(usage agent.Usage) error {
			return rt.do(context.Background(), func(rt *runtime) error { return rt.manager.SaveTraceBudget(context.Background(), tr.ID, usage) })
		})
		rt.active = frame
		go rt.runSegment(frame, in.ID)
		return
	}
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
	if !rt.closing && tr.State == "running" && runErr == nil && rt.manager.Fault() == nil {
		for _, id := range v.Follow {
			in := v.Inputs[id]
			if in.TraceID == tr.ID && in.State == "pending" {
				go rt.runSegment(frame, id)
				return
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
	if err := rt.finishInterruptedTurn(frame); err != nil && state == "completed" {
		state = "failed"
	}
	if rt.manager.Fault() == nil {
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			_ = rt.manager.SaveTraceError(context.Background(), tr.ID, runErr.Error())
		}
		_ = rt.manager.SetTraceState(context.Background(), tr.ID, state, terminal(state))
	}
	frame.cancel()
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
		return nil, model.NewError(model.CodeStateConflict, "session is closed")
	default:
	}
	select {
	case rt.mailbox <- cmd:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-rt.done:
		return nil, model.NewError(model.CodeStateConflict, "session is closed")
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
			return nil, model.NewError(model.CodeStateConflict, "session is closed")
		}
	}
}
func (rt *runtime) do(ctx context.Context, fn func(*runtime) error) error {
	_, err := rt.call(ctx, func(rt *runtime) (any, error) { return nil, fn(rt) })
	return err
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, agent.FrozenCall) (agent.Decision, error) {
	return agent.DecisionAllow, nil
}
func terminal(state string) bool {
	return state == "completed" || state == "cancelled" || state == "failed"
}
