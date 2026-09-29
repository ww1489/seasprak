package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

type todoResultProbe struct {
	effect agent.TodoEffect
	err    error
	calls  int
}

func (p *todoResultProbe) Update(context.Context, agent.AuthorizedTodo) (agent.TodoEffect, error) {
	p.calls++
	return p.effect, p.err
}
func TestTodosErrorProjection(t *testing.T) {
	for _, tc := range []struct {
		name               string
		effect             agent.TodoEffect
		err                error
		status, side, code string
		executed           bool
	}{
		{name: "permission", err: product.NewError(product.CodePermissionDenied, "private detail"), status: "failed", side: "none", code: product.CodePermissionDenied},
		{name: "cancel before commit", err: errors.Join(product.NewError(product.CodeStateConflict, "TODO rejected before commit"), context.Canceled), status: "cancelled", side: "none", code: product.CodeStateConflict},
		{name: "backend cancellation without evidence", err: context.Canceled, status: "cancelled", side: "unknown", code: "controlled TODO update failed"},
		{name: "append uncertain", err: product.NewError(product.CodeStorageUnavailable, "private detail"), status: "failed", side: "unknown", code: product.CodeStorageUnavailable},
		{name: "unclassified", err: errors.New("private detail"), status: "failed", side: "unknown", code: "controlled TODO update failed"},
		{name: "confirmed with error", effect: agent.TodoEffect{Confirmed: true}, err: product.NewError(product.CodeStorageUnavailable, "private detail"), status: "failed", side: "confirmed", code: product.CodeStorageUnavailable, executed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &todoResultProbe{effect: tc.effect, err: tc.err}
			e := &Executor{operations: Operations{Todos: p}}
			out := e.invokeTodos(t.Context(), agent.AuthorizedExecution{}, agent.FrozenExecution{})
			if out.Status != tc.status || out.SideEffect != tc.side || out.ExecutionError != tc.code || out.Executed != tc.executed || p.calls != 1 {
				t.Fatalf("out=%+v calls=%d", out, p.calls)
			}
		})
	}
}
