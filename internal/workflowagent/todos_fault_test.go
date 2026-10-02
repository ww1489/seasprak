package workflowagent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
)

type todoFaultStore struct {
	storage.Store
	kind     string
	persist  bool
	attempts atomic.Int32
}

func (s *todoFaultStore) Append(ctx context.Context, id string, expected storage.ExpectedCommit, c storage.Commit) (storage.CommitReceipt, error) {
	for _, r := range c.ControlRecords {
		if r.Type == s.kind {
			s.attempts.Add(1)
			if s.persist {
				if _, err := s.Store.Append(context.WithoutCancel(ctx), id, expected, c); err != nil {
					return storage.CommitReceipt{}, err
				}
			}
			return storage.CommitReceipt{}, context.Canceled
		}
	}
	return s.Store.Append(ctx, id, expected, c)
}

type todoBackendResult struct {
	effect agent.TodoEffect
	err    error
}

func TestWorkflowTodosAppendFaultUnknownAndDiskReopenNeverReexecutes(t *testing.T) {
	for _, tc := range []struct {
		name, kind          string
		persist             bool
		todos, observations int
	}{
		{"effect_append_unacknowledged", "workflow_todo", false, 0, 0},
		{"effect_saved_reply_lost", "workflow_todo", true, 1, 0},
		{"observation_append_unacknowledged", "workflow_tool_observation", false, 1, 0},
		{"observation_saved_reply_lost", "workflow_tool_observation", true, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := todoOptions(t, "once")
			w := newWorkflow(t, opts)
			backend := w.opts.Operations.Todos
			var updates atomic.Int32
			results := make(chan todoBackendResult, 1)
			// Observe the actual default backend without adding a production hook
			// or changing the run's committed default ownership manifest.
			w.opts.Operations.Todos = workflowTodoOperation(func(ctx context.Context, request agent.AuthorizedTodo) (agent.TodoEffect, error) {
				updates.Add(1)
				effect, err := backend.Update(ctx, request)
				results <- todoBackendResult{effect, err}
				return effect, err
			})
			fault := &todoFaultStore{Store: w.store, kind: tc.kind, persist: tc.persist}
			w.store = fault
			submit(t, w)
			paused := waitStopped(t, w)
			if updates.Load() != 0 || fault.attempts.Load() != 0 {
				t.Fatal("fault fixture executed before approval")
			}
			answerTodo(t, w, paused, "allowed-once")
			if updates.Load() != 0 || fault.attempts.Load() != 0 {
				t.Fatal("answer reached effect backend")
			}
			if _, err := w.Resume(t.Context(), WorkflowControlCommand{IdempotencyKey: "resume", Principal: "local"}); err != nil {
				t.Fatal(err)
			}
			waitExited(t, w)
			result := <-results
			if tc.kind == "workflow_todo" {
				requireTodoCode(t, result.err, product.CodeStorageUnavailable)
				if result.effect.Confirmed || errors.Is(result.err, context.Canceled) {
					t.Fatal("unacknowledged Append claimed confirmation or precommit cancellation")
				}
			} else if result.err != nil || !result.effect.Confirmed {
				t.Fatal("acknowledged TODO effect was discarded after observation failure")
			}
			w.mu.Lock()
			broken, unknown := w.broken, w.state.hasUnknown()
			w.mu.Unlock()
			requireTodoCode(t, broken, product.CodeStorageUnavailable)
			if !unknown || updates.Load() != 1 || fault.attempts.Load() != 1 {
				t.Fatal("effect fault lost unknown or retried backend/Append")
			}
			snapshot, _ := w.Snapshot(t.Context())
			if snapshot.ExecutionStopped || snapshot.State == "completed" || snapshot.Usage.ToolExecutions != 1 || snapshot.Usage.LogicalModelCalls != 0 {
				t.Fatal("fault published an uncommitted terminal state or refunded claim")
			}
			loaded := loadTodoRun(t, opts)
			if len(todoRecords(loaded, "workflow_todo")) != tc.todos || len(todoRecords(loaded, "workflow_tool_intent")) != 1 || len(todoRecords(loaded, "workflow_tool_observation")) != tc.observations {
				t.Fatal("wrong actual effect/claim/observation records")
			}
			_, err := w.Resume(t.Context(), WorkflowControlCommand{IdempotencyKey: "retry", Principal: "local"})
			requireTodoCode(t, err, product.CodeStorageUnavailable)
			requireTodoCode(t, w.Close(t.Context()), product.CodeStorageUnavailable)
			reopened, err := OpenWorkflowAgent(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(context.Background())
			after, _ := reopened.Snapshot(t.Context())
			if after.Revision != uint64(len(loaded.Commits)) || after.Usage != snapshot.Usage || updates.Load() != 1 || fault.attempts.Load() != 1 {
				t.Fatal("Open executed/refunded/modified recovered facts")
			}
			reopened.mu.Lock()
			unknown = reopened.state.hasUnknown()
			updatesRecovered := len(reopened.state.TodoUpdates)
			reopened.mu.Unlock()
			if unknown != (tc.observations == 0) || updatesRecovered != tc.todos {
				t.Fatal("replay concealed a missing observation or discarded committed TODO")
			}
			_, err = reopened.Resume(t.Context(), WorkflowControlCommand{IdempotencyKey: "retry-open", Principal: "local"})
			requireTodoCode(t, err, product.CodeIncompatibleResume)
			// Duplicate input returns the original acceptance receipt and never
			// converts a non-stopped recovered run into an automatic execution.
			submit(t, reopened)
			afterLoad := loadTodoRun(t, opts)
			if len(afterLoad.Commits) != len(loaded.Commits) || len(todoRecords(afterLoad, "workflow_todo")) != tc.todos || updates.Load() != 1 || fault.attempts.Load() != 1 {
				t.Fatal("reopen/explicit controls automatically replayed unknown TODO")
			}
		})
	}
}

func TestWorkflowTodosLostBackendResultPreservesUnknownObservation(t *testing.T) {
	opts := todoOptions(t, "")
	w := newWorkflow(t, opts)
	backend := w.opts.Operations.Todos
	var updates atomic.Int32
	w.opts.Operations.Todos = workflowTodoOperation(func(ctx context.Context, request agent.AuthorizedTodo) (agent.TodoEffect, error) {
		updates.Add(1)
		effect, err := backend.Update(ctx, request)
		if err != nil || !effect.Confirmed {
			return effect, err
		}
		// Lose the return value only after the real default backend acknowledged
		// its effect. The executor must save unknown, not fabricate no effect.
		return agent.TodoEffect{}, product.NewError(product.CodeStorageUnavailable, "synthetic backend result loss")
	})
	submit(t, w)
	final := waitStopped(t, w)
	loaded := loadTodoRun(t, opts)
	if final.State != "failed" || final.ErrorCode != product.CodeReconciliationRequired || updates.Load() != 1 || final.Usage.ToolExecutions != 1 || len(todoRecords(loaded, "workflow_todo")) != 1 || len(todoRecords(loaded, "workflow_tool_intent")) != 1 || len(final.Observations) != 1 {
		t.Fatal("lost backend result retried or concealed the original effect")
	}
	for _, observation := range final.Observations {
		if observation.Status != "failed" || observation.SideEffect != "unknown" || observation.Executed {
			t.Fatalf("unexpected lost-result observation: %+v", observation)
		}
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	if !reopened.state.hasUnknown() || len(reopened.state.TodoUpdates) != 1 || reopened.state.Revision != final.Revision || updates.Load() != 1 {
		t.Fatal("reopen lost unknown or replayed the effect")
	}
	_, err = reopened.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
	requireCode(t, err, product.CodeIncompatibleResume)
	if len(todoRecords(loadTodoRun(t, opts), "workflow_todo")) != 1 || updates.Load() != 1 {
		t.Fatal("unknown observation was automatically retried")
	}
}

type todoBlockingAppendStore struct {
	storage.Store
	entered chan struct{}
	release chan struct{}
}

func (s *todoBlockingAppendStore) Append(ctx context.Context, id string, expected storage.ExpectedCommit, c storage.Commit) (storage.CommitReceipt, error) {
	for _, r := range c.ControlRecords {
		if r.Type == "workflow_todo" {
			close(s.entered)
			<-s.release
			return s.Store.Append(context.WithoutCancel(ctx), id, expected, c)
		}
	}
	return s.Store.Append(ctx, id, expected, c)
}

func TestWorkflowTodosCancellationDuringAppendAwaitsDefinitiveReceipt(t *testing.T) {
	w, counter, call, frozen := pendingTodoOwner(t)
	validator := &todoTicketValidator{owner: w}
	request := claimTodoRequest(t, w, call, frozen, validator)
	blocked := &todoBlockingAppendStore{Store: w.store, entered: make(chan struct{}), release: make(chan struct{})}
	w.store = blocked
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan todoBackendResult, 1)
	go func() {
		effect, err := w.opts.Operations.Todos.Update(ctx, request)
		result <- todoBackendResult{effect, err}
	}()
	<-blocked.entered
	cancel()
	select {
	case <-result:
		t.Fatal("in-flight Append returned cancellation as zero-effect proof")
	default:
	}
	close(blocked.release)
	actual := <-result
	if actual.err != nil || !actual.effect.Confirmed || actual.effect.Version != "1" || counter.appends != 2 || validator.calls.Load() != 1 || len(w.state.TodoUpdates) != 1 {
		t.Fatalf("definitive acknowledged effect lost after cancellation: %+v err=%v", actual.effect, actual.err)
	}
}
