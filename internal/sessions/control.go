package sessions

import (
	"context"
	"encoding/json"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

// Durable operation kinds accepted through the keyed control entry points.
const (
	opCancelTrace   = "cancel_trace"
	opContinueQueue = "continue_queue"
)

// CancelTraceRequest asks to stop a trace. ExpectedRevision is optional; when
// set it must equal the current session revision.
type CancelTraceRequest struct {
	TraceID          string
	IdempotencyKey   string
	ExpectedRevision *uint64
}

// ContinueQueueRequest releases the hold of the listed queued traces
// atomically; it never preempts active work.
type ContinueQueueRequest struct {
	TraceIDs         []string
	IdempotencyKey   string
	ExpectedRevision *uint64
}

// jsonStrings keeps array order significant in the idempotency digest.
func jsonStrings(v []string) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, product.NewError(product.CodeInvalidArgument, "invalid trace ids")
	}
	return raw, nil
}

func revision(p *uint64) (uint64, bool) {
	if p == nil {
		return 0, false
	}
	return *p, true
}

// CancelTrace durably accepts cancellation before signalling the worker. The
// receipt proves acceptance only; the operation completes after the trace
// reaches a terminal state, with ResultRef set to that final state.
func (s *AgentSession) CancelTrace(ctx context.Context, req CancelTraceRequest) (state.OperationReceipt, error) {
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		rev, check := revision(req.ExpectedRevision)
		cmd := state.OperationCommand{Principal: rt.opts.Principal, Kind: opCancelTrace, Target: req.TraceID, IdempotencyKey: req.IdempotencyKey, ExpectedRevision: rev}
		if receipt, found, err := rt.manager.FindOperation(cmd); found || err != nil {
			return receipt, err
		}
		tr := rt.manager.View().Traces[req.TraceID]
		if tr == nil {
			return nil, product.NewError(product.CodeNotFound, "trace not found")
		}
		active := rt.active != nil && rt.active.scope.TraceID == req.TraceID
		next := ""
		switch {
		case terminal(tr.State):
		case active:
			next = "cancelling"
		case !tr.Started:
			next = "cancelled"
		default:
			if tr.State != "paused" && tr.State != "cancelling" {
				return nil, product.NewError(product.CodeStateConflict, "trace is not interrupted")
			}
			if !tr.ExecutionStopped && rt.manager.View().TraceHasUnresolvedEffects(tr.ID) {
				return nil, product.NewError(product.CodeReconciliationRequired, "interrupted execution has no stopped proof")
			}
			next = "cancelling"
		}
		receipt, duplicate, err := rt.manager.AcceptTraceControl(ctx, cmd, check, req.TraceID, next)
		if err != nil || duplicate {
			return receipt, err
		}
		// Side effects start only after the accepted operation is durable.
		switch {
		case active:
			rt.active.cancel()
		case next == "cancelling":
			if err := rt.cancelInterrupted(context.WithoutCancel(ctx), rt.manager.View().Traces[req.TraceID]); err != nil {
				return receipt, err
			}
		}
		rt.settleTraceOperations(req.TraceID)
		if next == "cancelled" {
			rt.schedule()
		}
		return receipt, nil
	})
	if err != nil {
		if receipt, ok := value.(state.OperationReceipt); ok && receipt.OperationID != "" {
			return receipt, err
		}
		return state.OperationReceipt{}, err
	}
	return value.(state.OperationReceipt), nil
}

// ContinueQueued validates every target first, then releases all holds and
// records the accepted operation in one commit before scheduling.
func (s *AgentSession) ContinueQueued(ctx context.Context, req ContinueQueueRequest) (state.OperationReceipt, error) {
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		if len(req.TraceIDs) == 0 {
			return nil, product.NewError(product.CodeInvalidArgument, "traceIds are required")
		}
		rev, check := revision(req.ExpectedRevision)
		content, err := jsonStrings(req.TraceIDs)
		if err != nil {
			return nil, err
		}
		cmd := state.OperationCommand{Principal: rt.opts.Principal, Kind: opContinueQueue, Target: "queue", IdempotencyKey: req.IdempotencyKey, ExpectedRevision: rev, Content: content}
		if receipt, found, err := rt.manager.FindOperation(cmd); found || err != nil {
			return receipt, err
		}
		view := rt.manager.View()
		seen := map[string]bool{}
		for _, id := range req.TraceIDs {
			tr := view.Traces[id]
			if seen[id] {
				return nil, product.NewError(product.CodeInvalidArgument, "duplicate trace id")
			}
			seen[id] = true
			if tr == nil || tr.State != "queued" {
				return nil, product.NewError(product.CodeStateConflict, "only queued trace can continue")
			}
			if tr.Kind == "command" {
				return nil, incompatibleResume("legacy direct commands cannot continue")
			}
			if tr.Generation != rt.generation {
				return nil, product.NewError(product.CodeIncompatibleVersion, "queued generation cannot be replaced")
			}
			if err := rt.executable(tr); err != nil {
				return nil, err
			}
		}
		if view.HasUnresolvedEffects() {
			return nil, product.NewError(product.CodeReconciliationRequired, "unresolved tool effects block queued work")
		}
		if active := view.Traces[view.ActiveTrace]; active != nil && active.State == "paused" {
			return nil, product.NewError(product.CodeReconciliationRequired, "interrupted execution blocks queued work")
		}
		receipt, duplicate, err := rt.manager.AcceptQueueRelease(ctx, cmd, check, req.TraceIDs)
		if err != nil || duplicate {
			return receipt, err
		}
		if err := rt.manager.TransitionOperation(context.WithoutCancel(ctx), receipt.OperationID, 1, "completed", "released", ""); err != nil {
			return receipt, err
		}
		rt.schedule()
		return receipt, nil
	})
	if err != nil {
		if receipt, ok := value.(state.OperationReceipt); ok && receipt.OperationID != "" {
			return receipt, err
		}
		return state.OperationReceipt{}, err
	}
	return value.(state.OperationReceipt), nil
}

// settleTraceOperations completes accepted cancel operations once their trace
// is terminal. ResultRef records the actual final state, which may be
// "completed" when natural completion won the race.
func (rt *runtime) settleTraceOperations(traceID string) {
	if rt.manager.Fault() != nil {
		return
	}
	v := rt.manager.View()
	tr := v.Traces[traceID]
	if tr == nil || !terminal(tr.State) {
		return
	}
	for id, op := range v.Operations {
		if op.Kind == opCancelTrace && op.Receipt.Target == traceID && op.State == "accepted" {
			_ = rt.manager.TransitionOperation(context.Background(), id, op.Revision, "completed", tr.State, "")
		}
	}
}

// settleAllTraceOperations repairs the window where a trace became terminal
// but the process stopped before its cancel operation was completed.
func (rt *runtime) settleAllTraceOperations() {
	v := rt.manager.View()
	for _, op := range v.Operations {
		if op.Kind == opCancelTrace && op.State == "accepted" {
			rt.settleTraceOperations(op.Receipt.Target)
		}
	}
}
