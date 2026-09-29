package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync/atomic"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestTodosSessionPersistsReplacesAndIsolatesOnDisk(t *testing.T) {
	first := `{"items":[{"title":"first","state":"pending"}]}`
	second := `{"items":[{"title":"second","state":"completed"}]}`
	third := `{"items":[]}`
	model := testkit.NewFake(
		testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "same-provider", Name: "write_todos", Arguments: first}}},
		testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "same-provider", Name: "write_todos", Arguments: second}}},
		testkit.Step{Text: "done"},
		testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "same-provider", Name: "write_todos", Arguments: third}}},
		testkit.Step{Text: "done"},
	)
	opts := Options{SessionID: "todos-disk", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: model, Tools: []tools.Definition{builtinDefinitionForSession(t, "write_todos")}, ResourceScheduler: tools.NewResourceScheduler()}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	for i := 0; i < 2; i++ {
		r := submitOutput(t, s)
		waitResumeCondition(t, func() bool {
			tr := s.rt.manager.View().Traces[r.TraceID]
			return terminal(tr.State) && tr.ExecutionStopped
		})
	}
	v := s.rt.manager.View()
	if len(v.Calls) != 3 || model.Calls() != 5 {
		t.Fatalf("calls=%d model=%d", len(v.Calls), model.Calls())
	}
	invocationCalls := map[string]int{}
	for _, call := range v.Calls {
		if !call.Claimed || call.Observation == nil || call.Observation.Status != "succeeded" || !call.Observation.Executed || call.Observation.SideEffect != "confirmed" || !json.Valid([]byte(call.Observation.Content)) {
			t.Fatalf("TODO was not committed: %+v", call)
		}
		invocationCalls[call.Scope.InvocationID]++
	}
	if len(invocationCalls) != 2 || len(v.Todos) != 2 || len(v.TodoUpdates) != 3 {
		t.Fatal("invocations were not isolated")
	}
	for invocation, count := range invocationCalls {
		latest := v.Todos[invocation]
		want := third
		if count == 2 {
			want = second
		}
		var gotJSON, wantJSON any
		_ = json.Unmarshal(latest.Content, &gotJSON)
		_ = json.Unmarshal([]byte(want), &wantJSON)
		if latest.Version != uint64(count) || !reflect.DeepEqual(gotJSON, wantJSON) {
			t.Fatal("TODO replacement semantics lost")
		}
	}
	// Reusing an older committed call returns its original receipt and never
	// rewrites the current list, even after the invocation has finished.
	for id, update := range v.TodoUpdates {
		effect, err := s.rt.manager.UpdateTodos(t.Context(), v.FrozenExecutions["execution:"+id])
		if err != nil || effect != update.Effect() || s.rt.manager.View().LastSeq != v.LastSeq {
			t.Fatal("receipt replay wrote or changed result", err)
		}
	}
	loaded, err := s.rt.opts.Store.Load(t.Context(), opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	updates := 0
	for _, c := range loaded.Commits {
		for _, r := range c.ControlRecords {
			if r.Type == "todo_update" {
				updates++
			}
		}
	}
	if updates != 3 {
		t.Fatalf("durable TODO updates=%d, want 3", updates)
	}
	finished := 0
	for _, event := range v.Events {
		if event.Type == "tool.finished" {
			finished++
		}
	}
	if finished != 3 {
		t.Fatalf("TODO finished events=%d, want exactly one per call", finished)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(journalPath(opts.StateRoot, opts.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	opts.ReadOnly = true
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close(context.Background())
	restored := opened.rt.manager.View()
	if !reflect.DeepEqual(v.Calls, restored.Calls) || !reflect.DeepEqual(v.Todos, restored.Todos) || !reflect.DeepEqual(v.TodoUpdates, restored.TodoUpdates) || model.Calls() != 5 {
		t.Fatal("reopen lost facts or executed model")
	}
	after, err := os.ReadFile(journalPath(opts.StateRoot, opts.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only reopen wrote journal")
	}
}

// todoFaultStore fails only the TODO commit, optionally after the real disk
// append. This simulates a lost durability reply without replaying execution.
type todoFaultStore struct {
	store.Store
	after   bool
	updates atomic.Int32
}

func (s *todoFaultStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	for _, r := range c.ControlRecords {
		if r.Type == "todo_update" {
			s.updates.Add(1)
			if s.after {
				if _, err := s.Store.Append(ctx, id, expected, c); err != nil {
					return store.CommitReceipt{}, err
				}
				// Cancellation after a durable append must remain unknown to the
				// live instance, not be projected as a pre-start rejection.
				return store.CommitReceipt{}, context.Canceled
			}
			return store.CommitReceipt{}, errors.New("controlled TODO append fault")
		}
	}
	return s.Store.Append(ctx, id, expected, c)
}
func TestTodosCommitFailureAndLostReplyReopen(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-append", true: "after-append"}[after], func(t *testing.T) {
			model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "todo", Name: "write_todos", Arguments: `{"items":[]}`}}}, testkit.Step{Text: "must not execute"})
			opts := Options{SessionID: "todo-fault", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: model, Tools: []tools.Definition{builtinDefinitionForSession(t, "write_todos")}, ResourceScheduler: tools.NewResourceScheduler()}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			fault := &todoFaultStore{Store: s.rt.opts.Store, after: after}
			err = s.rt.do(t.Context(), func(rt *runtime) error {
				m, err := state.NewManager(fault, opts.SessionID)
				if err != nil {
					return err
				}
				rt.manager = m
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			r := submitOutput(t, s)
			waitResumeCondition(t, func() bool {
				value, err := s.rt.call(t.Context(), func(rt *runtime) (any, error) { return rt.active == nil && rt.manager.Fault() != nil, nil })
				return err == nil && value.(bool)
			})
			v := s.rt.manager.View()
			if len(v.Todos) != 0 || len(v.TodoUpdates) != 0 || fault.updates.Load() != 1 || model.Calls() != 1 {
				t.Fatalf("failed commit published state or reran: updates=%d model=%d", fault.updates.Load(), model.Calls())
			}
			pe, ok := product.AsError(s.rt.manager.Fault())
			if !ok || pe.Code != product.CodeStorageUnavailable {
				t.Fatal("storage failure code lost")
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(journalPath(opts.StateRoot, opts.SessionID))
			if err != nil {
				t.Fatal(err)
			}
			opts.ReadOnly = true
			opts.ResourceScheduler = tools.NewResourceScheduler()
			opened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close(context.Background())
			restored := opened.rt.manager.View()
			want := 0
			if after {
				want = 1
			}
			if len(restored.TodoUpdates) != want || model.Calls() != 1 || fault.updates.Load() != 1 {
				t.Fatal("reopen did not reconstruct acknowledged disk prefix")
			}
			if after {
				for id := range restored.Calls {
					if opts.ResourceScheduler.HasHold(tools.ResourceHoldID(opts.SessionID, id)) {
						t.Fatal("known committed TODO restored an unknown-effect hold")
					}
				}
				if restored.TraceHasUnresolvedEffects(r.TraceID) {
					t.Fatal("committed TODO receipt did not recover original observation")
				}
				for _, call := range restored.Calls {
					if call.Observation == nil || call.Observation.Status != "succeeded" || !call.Observation.Executed {
						t.Fatal("lost result was not recovered from TODO fact")
					}
				}
			} else if !restored.TraceHasUnresolvedEffects(r.TraceID) {
				t.Fatal("missing TODO commit falsely proved an effect")
			}
			disk, err := os.ReadFile(journalPath(opts.StateRoot, opts.SessionID))
			if err != nil || !bytes.Equal(before, disk) {
				t.Fatal("read-only recovery wrote disk", err)
			}
		})
	}
}

func TestTodosResumeRejectsOwnershipChangeAfterDiskOpen(t *testing.T) {
	f := pausedResumeFixture(t, true, resumeFixtureOptions{Disk: true})
	beforeCalls, beforeRuns := f.model.Calls(), f.runs.Load()
	if err := f.s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	host := &todoInjectedProbe{}
	f.opts.Operations.Todos = host
	opened, err := OpenAgentSession(t.Context(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close(context.Background())
	before := opened.rt.manager.View()
	_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeIncompatibleResume || host.calls != 0 || f.model.Calls() != beforeCalls || f.runs.Load() != beforeRuns {
		t.Fatal("backend ownership changed silently on resume", err)
	}
}

func TestTodosResumeFingerprintBindsOwnership(t *testing.T) {
	opts := Options{Model: versionedPauseModel{}, GenerationFingerprint: "todos-build"}
	baseline := resumeBuildFingerprint(opts)
	opts.Operations.Todos = &sessionTodos{}
	if baseline == "" || baseline != resumeBuildFingerprint(opts) {
		t.Fatal("default assembly changed backend identity")
	}
	opts.Operations.Todos = &todoInjectedProbe{}
	if baseline == resumeBuildFingerprint(opts) {
		t.Fatal("switching TODO owner remained resume compatible")
	}
}

var _ agent.TodoOperations = (*todoInjectedProbe)(nil)

type todoInjectedProbe struct{ calls int }

func (p *todoInjectedProbe) Update(ctx context.Context, r agent.AuthorizedTodo) (agent.TodoEffect, error) {
	if err := r.Authorization.Validate(ctx); err != nil {
		return agent.TodoEffect{}, err
	}
	p.calls++
	return agent.TodoEffect{Version: "host-v1", Content: `{"host":true}`, Confirmed: true}, nil
}
func TestTodosInjectedBackendIsNotWrapped(t *testing.T) {
	p := &todoInjectedProbe{}
	model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "todo", Name: "write_todos", Arguments: `{"items":[]}`}}}, testkit.Step{Text: "done"})
	s, err := CreateAgentSession(t.Context(), Options{SessionID: "todos-injected", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: model, Tools: []tools.Definition{builtinDefinitionForSession(t, "write_todos")}, Operations: tools.Operations{Todos: p}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	r := submitOutput(t, s)
	waitResumeCondition(t, func() bool {
		tr := s.rt.manager.View().Traces[r.TraceID]
		return terminal(tr.State) && tr.ExecutionStopped
	})
	loaded, err := s.rt.opts.Store.Load(t.Context(), "todos-injected")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range loaded.Commits {
		for _, r := range c.ControlRecords {
			if r.Type == "todo_update" {
				t.Fatal("host TODO was silently double-written")
			}
		}
	}
	if s.rt.opts.Operations.Todos != p || p.calls != 1 {
		t.Fatalf("injected owner replaced or calls=%d", p.calls)
	}
}
