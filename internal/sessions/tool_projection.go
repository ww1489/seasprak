package sessions

import (
	"context"
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

func (rt *runtime) saveToolProjection(ctx context.Context, scope agent.ExecutionScope, payload json.RawMessage) error {
	var p agent.ToolOutputProjection
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	call, ok := rt.manager.View().Calls[p.CallID]
	if !ok || call.Scope.SessionID != scope.SessionID || call.Scope.TraceID != scope.TraceID || call.Scope.InvocationID != scope.InvocationID || call.Scope.TurnID != scope.TurnID {
		return product.NewError(product.CodeStateConflict, "tool projection is outside execution scope")
	}
	environment, _ := rt.resourceDomain()
	if p.Artifact.ID != "" && p.Artifact.Environment != environment {
		return product.NewError(product.CodeStateConflict, "tool projection artifact environment changed")
	}
	return rt.manager.SaveToolProjection(context.WithoutCancel(ctx), p)
}

func (rt *runtime) LookupToolProjection(ctx context.Context, scope agent.ExecutionScope, callID string) (*agent.ToolOutputProjection, error) {
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		if !rt.matchesToolExecution(scope) {
			return nil, product.NewError(product.CodeStateConflict, "tool projection execution is not active")
		}
		scope = rt.ownTurn(scope)
		view := rt.manager.View()
		call, ok := view.Calls[callID]
		if !ok || call.Scope.SessionID != scope.SessionID || call.Scope.TraceID != scope.TraceID || call.Scope.InvocationID != scope.InvocationID || call.Scope.TurnID != scope.TurnID {
			return nil, product.NewError(product.CodeStateConflict, "tool projection call is outside execution scope")
		}
		p, ok := view.ToolProjections[callID]
		if !ok {
			return (*agent.ToolOutputProjection)(nil), nil
		}
		return &p, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*agent.ToolOutputProjection), nil
}
