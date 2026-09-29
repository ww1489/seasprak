package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

type todoBoundaryProbe struct {
	update func(context.Context, agent.AuthorizedTodo) (agent.TodoEffect, error)
	calls  atomic.Int32
}

func (p *todoBoundaryProbe) Update(ctx context.Context, r agent.AuthorizedTodo) (agent.TodoEffect, error) {
	p.calls.Add(1)
	return p.update(ctx, r)
}

func TestTodosAuthorizationBindingAndOneUse(t *testing.T) {
	model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "todo", Name: "write_todos", Arguments: `{"items":[]}`}}}, testkit.Step{Text: "done"})
	s, err := CreateAgentSession(t.Context(), Options{SessionID: "todo-authority", Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{builtinDefinitionForSession(t, "write_todos")}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	backend := s.rt.opts.Operations.Todos
	probe := &todoBoundaryProbe{}
	probe.update = func(ctx context.Context, r agent.AuthorizedTodo) (agent.TodoEffect, error) {
		seq := s.rt.manager.View().LastSeq
		for _, mutate := range []func(*agent.AuthorizedTodo){
			func(r *agent.AuthorizedTodo) { r.InvocationID = "other" },
			func(r *agent.AuthorizedTodo) {
				r.Content = json.RawMessage(`{"items":[{"title":"other","state":"pending"}]}`)
			},
			func(r *agent.AuthorizedTodo) {
				r.Authorization = agent.AuthorizedExecution{Frozen: r.Authorization.Frozen}
			},
		} {
			changed := r
			mutate(&changed)
			effect, err := backend.Update(ctx, changed)
			pe, ok := product.AsError(err)
			if !ok || pe.Code != product.CodePermissionDenied || effect.Confirmed || s.rt.manager.View().LastSeq != seq {
				t.Error("invalid binding consumed authority or committed", err)
			}
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		effect, err := backend.Update(canceled, r)
		if !errors.Is(err, context.Canceled) || effect.Confirmed || s.rt.manager.View().LastSeq != seq {
			t.Error("cancelled request wrote state", err)
		}
		effect, err = backend.Update(ctx, r)
		if err != nil {
			return effect, err
		}
		after := s.rt.manager.View().LastSeq
		lookup, lookupErr := s.rt.LookupTool(ctx, r.Authorization.Frozen.Scope, r.Authorization.Frozen.ProviderCallID)
		if lookupErr != nil || lookup.Observation == nil || lookup.Observation.Content != effect.Content || !lookup.Observation.Executed {
			t.Error("committed TODO receipt was unavailable before executor result save", lookupErr)
		}
		duplicate, err := backend.Update(ctx, r)
		pe, ok := product.AsError(err)
		if !ok || pe.Code != product.CodeStateConflict || duplicate.Confirmed || s.rt.manager.View().LastSeq != after {
			t.Error("used ticket was accepted", err)
		}
		return effect, nil
	}
	if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.opts.Operations.Todos = probe; return nil }); err != nil {
		t.Fatal(err)
	}
	r := submitOutput(t, s)
	waitResumeCondition(t, func() bool {
		tr := s.rt.manager.View().Traces[r.TraceID]
		return terminal(tr.State) && tr.ExecutionStopped
	})
	v := s.rt.manager.View()
	if probe.calls.Load() != 1 || len(v.TodoUpdates) != 1 || model.Calls() != 2 || v.Traces[r.TraceID].Usage.ToolExecutions != 1 {
		t.Fatal("authorization checks changed invocation count")
	}
}

func TestTodosCancellationBeforeCommitDoesNotPublish(t *testing.T) {
	model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "todo", Name: "write_todos", Arguments: `{"items":[]}`}}}, testkit.Step{Text: "done"})
	s, err := CreateAgentSession(t.Context(), Options{SessionID: "todo-cancel", Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{builtinDefinitionForSession(t, "write_todos")}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	backend := s.rt.opts.Operations.Todos
	probe := &todoBoundaryProbe{update: func(ctx context.Context, r agent.AuthorizedTodo) (agent.TodoEffect, error) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		return backend.Update(canceled, r)
	}}
	if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.opts.Operations.Todos = probe; return nil }); err != nil {
		t.Fatal(err)
	}
	r := submitOutput(t, s)
	waitResumeCondition(t, func() bool {
		tr := s.rt.manager.View().Traces[r.TraceID]
		return terminal(tr.State) && tr.ExecutionStopped
	})
	v := s.rt.manager.View()
	if probe.calls.Load() != 1 || len(v.TodoUpdates) != 0 || len(v.Todos) != 0 {
		t.Fatal("cancelled update was applied")
	}
	for _, call := range v.Calls {
		if !call.Claimed || call.Observation == nil || call.Observation.Status != "cancelled" || call.Observation.Executed || call.Observation.SideEffect != "none" {
			t.Fatalf("cancelled TODO projection=%+v", call.Observation)
		}
	}
}
