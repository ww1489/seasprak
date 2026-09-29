package sessions

import (
	"bytes"
	"context"
	"errors"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

// sessionTodos owns only the built-in journal backend. Host injection replaces
// it completely; the session never wraps or mirrors a host's TODO updates.
type sessionTodos struct{ rt *runtime }

func (s *sessionTodos) Update(ctx context.Context, request agent.AuthorizedTodo) (agent.TodoEffect, error) {
	frozen := request.Authorization.Frozen.Clone()
	if request.InvocationID == "" || request.InvocationID != frozen.Scope.InvocationID || !bytes.Equal(request.Content, frozen.FinalArguments) || frozen.Tool != "write_todos" || frozen.BackendID != "todo-operations" {
		return agent.TodoEffect{}, product.NewError(product.CodePermissionDenied, "TODO request differs from its authorized execution")
	}
	// Validate calls the session validator itself; doing so inside the mailbox
	// would deadlock. Consume once outside, then recheck current state inside.
	if err := request.Authorization.Validate(ctx); err != nil {
		return agent.TodoEffect{}, todoPrecommitError(err)
	}
	// Once queued, await the definitive commit result. Returning ctx.Err while
	// Append is in flight would falsely report no effect for a committed update.
	value, err := s.rt.call(context.WithoutCancel(ctx), func(rt *runtime) (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		decision, err := rt.checkToolPolicyState(ctx, frozen.Scope, frozen, true)
		if err != nil {
			return nil, err
		}
		if decision != agent.DecisionAllow {
			return nil, product.NewError(product.CodePermissionDenied, "TODO execution is no longer allowed")
		}
		return rt.manager.UpdateTodos(ctx, frozen)
	})
	if err != nil {
		return agent.TodoEffect{}, todoPrecommitError(err)
	}
	return value.(agent.TodoEffect), nil
}

// Storage failures from UpdateTodos are normalized to storage_unavailable,
// even when Append returns cancellation. Thus a cancellation reaching here is
// provably before Append; preserve errors.Is while making that rejection explicit.
func todoPrecommitError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(product.NewError(product.CodeStateConflict, "TODO cancelled before commit"), err)
	}
	return err
}

func todoBackendIdentity(backend agent.TodoOperations) string {
	if backend == nil {
		return "session-journal-v1"
	}
	if _, ok := backend.(*sessionTodos); ok {
		return "session-journal-v1"
	}
	// The concrete injected implementation version remains covered by the host's
	// GenerationFingerprint; this tag additionally forbids switching ownership.
	return "host-injected"
}
