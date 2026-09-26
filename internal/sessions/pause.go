package sessions

import (
	"context"

	"github.com/cloudwego/eino/adk"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// GetOperation reads the durable operation status without starting execution.
func (s *AgentSession) GetOperation(ctx context.Context, id string) (state.OperationStatus, error) {
	if err := ctx.Err(); err != nil {
		return state.OperationStatus{}, err
	}
	return s.rt.manager.GetOperation(id)
}

// Pause requests a cooperative checkpoint of the active trace. The returned
// receipt is the durable accepted operation, never proof before worker exit.
func (s *AgentSession) Pause(ctx context.Context, traceID string) (state.OperationReceipt, error) {
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		tr := rt.manager.View().Traces[traceID]
		if tr == nil {
			return nil, product.NewError(product.CodeNotFound, "trace not found")
		}
		if rt.active == nil || rt.active.scope.TraceID != traceID || tr.State != "running" || rt.active.pauseID != "" || rt.active.ctx.Err() != nil || rt.manager.View().HasUnresolvedEffects() {
			return nil, product.NewError(product.CodeStateConflict, "trace is not actively pausable")
		}
		if _, ok := rt.opts.Store.(store.CheckpointBlobs); !ok {
			return nil, product.NewError(product.CodeStorageUnavailable, "checkpoint blob storage is unavailable")
		}
		receipt, err := rt.manager.AcceptOperation(ctx, state.OperationCommand{Principal: rt.opts.Principal, Kind: "pause", Target: traceID, ExpectedRevision: rt.manager.View().LastSeq})
		if err != nil {
			return nil, err
		}
		frame := rt.active
		frame.pauseID = receipt.OperationID
		if frame.loop != nil {
			frame.loop.Stop(adk.WithGraceful())
		}
		return struct {
			receipt state.OperationReceipt
			done    chan struct{}
		}{receipt, frame.done}, nil
	})
	if err != nil {
		return state.OperationReceipt{}, err
	}
	result := value.(struct {
		receipt state.OperationReceipt
		done    chan struct{}
	})
	select {
	case <-result.done:
		status, statusErr := s.rt.manager.GetOperation(result.receipt.OperationID)
		if statusErr != nil {
			return result.receipt, statusErr
		}
		if status.State != "completed" {
			if fault := s.rt.manager.Fault(); fault != nil {
				return result.receipt, fault
			}
			return result.receipt, product.NewError(product.CodeStateConflict, "pause did not produce a safe checkpoint")
		}
		return result.receipt, nil
	case <-ctx.Done():
		return result.receipt, ctx.Err()
	}
}
