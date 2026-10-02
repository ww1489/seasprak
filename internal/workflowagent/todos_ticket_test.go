package workflowagent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
)

// The prefix is produced by the real builtin/factory/executor. A failing intent
// Append stops it before an effect; the probe restores that frozen prefix into
// a fresh owner and then uses the real ledger/owner claim and ticket issuance.
func pendingTodoOwner(t *testing.T) (*WorkflowAgent, *migrationAppendCounter, agent.ToolRecord, agent.FrozenExecution) {
	t.Helper()
	opts := todoOptions(t, "")
	seed := injectStore(t, &opts)
	seed.reject = func(c storage.Commit) error {
		for _, r := range c.ControlRecords {
			if r.Type == "workflow_tool_intent" {
				return product.NewError(product.CodeStorageUnavailable, "fixture stops before TODO claim")
			}
		}
		return nil
	}
	original := newWorkflow(t, opts)
	submit(t, original)
	waitExited(t, original)
	loaded := loadTodoRun(t, opts)
	found := false
	for i, c := range loaded.Commits {
		for _, r := range c.ControlRecords {
			if r.Type == "workflow_frozen" {
				loaded.Commits = loaded.Commits[:i+1]
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found || len(todoRecords(loaded, "workflow_todo")) != 0 {
		t.Fatal("fixture did not stop before TODO effect")
	}
	s, err := replay(loaded, opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
	mem, err := memory.Open(opts.RunID, loaded.Header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mem.Close() })
	for _, c := range loaded.Commits {
		if _, err := mem.Append(t.Context(), opts.RunID, storage.ExpectedCommit{ExpectedPreviousSeq: c.ExpectedPreviousSeq}, c); err != nil {
			t.Fatal(err)
		}
	}
	counter := &migrationAppendCounter{Store: mem}
	probe := assemble(opts, counter, nil, s.Initial.BindingVersion)
	probe.state = s
	var call agent.ToolRecord
	for _, call = range s.Calls {
	}
	frozen := s.Frozen["execution:"+call.Call.CallID]
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	probe.active = &segment{scope: call.Scope, ctx: ctx, cancel: cancel}
	return probe, counter, call, frozen
}

type todoTicketValidator struct {
	owner *WorkflowAgent
	calls atomic.Int32
	after func(context.Context)
}

func (v *todoTicketValidator) ValidateExecutionTicket(ctx context.Context, frozen agent.FrozenExecution) error {
	v.calls.Add(1)
	// Both APIs take the owner mutex. Update must not hold it during Validate.
	if _, err := v.owner.Snapshot(ctx); err != nil {
		return err
	}
	if err := v.owner.ValidateExecutionTicket(ctx, frozen); err != nil {
		return err
	}
	if v.after != nil {
		v.after(ctx)
	}
	return nil
}

func claimTodoRequest(t *testing.T, w *WorkflowAgent, call agent.ToolRecord, frozen agent.FrozenExecution, validator agent.ExecutionTicketValidator) agent.AuthorizedTodo {
	t.Helper()
	ledger := agent.NewBudget(w.opts.Limits)
	ledger.Restore(w.state.Nodes[call.Scope.NodeExecutionID].Usage)
	receipt, err := ledger.ClaimTool(t.Context(), w, call.Scope, call.Call, frozen)
	if err != nil {
		t.Fatal(err)
	}
	tickets, err := agent.NewExecutionTickets()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := tickets.Issue(receipt, frozen, time.Now().Add(time.Minute), validator)
	if err != nil {
		t.Fatal(err)
	}
	return agent.AuthorizedTodo{Authorization: agent.NewAuthorizedExecution(ref, frozen), InvocationID: frozen.Scope.InvocationID, Content: append(json.RawMessage(nil), frozen.FinalArguments...)}
}

func TestWorkflowTodosTicketValidationOutsideLockOnce(t *testing.T) {
	w, counter, call, frozen := pendingTodoOwner(t)
	validator := &todoTicketValidator{owner: w}
	request := claimTodoRequest(t, w, call, frozen, validator)
	before := counter.appends
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result := make(chan todoBackendResult, 1)
	go func() {
		effect, err := w.opts.Operations.Todos.Update(ctx, request)
		result <- todoBackendResult{effect, err}
	}()
	var effect agent.TodoEffect
	var err error
	select {
	case actual := <-result:
		effect, err = actual.effect, actual.err
	case <-ctx.Done():
		t.Fatal("TODO validation deadlocked while re-entering the owner mutex")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !effect.Confirmed || effect.Version != "1" || effect.Content != call.Call.Arguments || validator.calls.Load() != 1 || counter.appends != before+1 || len(w.state.TodoUpdates) != 1 || w.state.Calls[call.Call.CallID].Observation != nil {
		t.Fatalf("TODO validation/commit contract: effect=%+v validators=%d appends=%d", effect, validator.calls.Load(), counter.appends-before)
	}
	// The same opaque authority cannot create a second version or effect.
	second, err := w.opts.Operations.Todos.Update(t.Context(), request)
	requireTodoCode(t, err, product.CodePermissionDenied)
	if second.Confirmed || counter.appends != before+1 || w.state.Todos[request.InvocationID].Version != 1 {
		t.Fatal("one-use authority repeated TODO effect")
	}
}

func TestWorkflowTodosCrossRunTicketCannotTransferAuthority(t *testing.T) {
	source, sourceStore, call, frozen := pendingTodoOwner(t)
	target, targetStore, _, _ := pendingTodoOwner(t)
	validator := &todoTicketValidator{owner: source}
	request := claimTodoRequest(t, source, call, frozen, validator)
	before := target.state.clone()
	appends := targetStore.appends
	effect, err := target.opts.Operations.Todos.Update(t.Context(), request)
	requireTodoCode(t, err, product.CodePermissionDenied)
	if effect.Confirmed || validator.calls.Load() != 0 || targetStore.appends != appends || !reflect.DeepEqual(before, target.state) {
		t.Fatal("cross-run request consumed source authority, wrote, or changed target")
	}
	// A foreign request is rejected before consuming its source's ticket.
	beforeSource := sourceStore.appends
	effect, err = source.opts.Operations.Todos.Update(t.Context(), request)
	if err != nil || !effect.Confirmed || validator.calls.Load() != 1 || sourceStore.appends != beforeSource+1 || len(target.state.TodoUpdates) != 0 {
		t.Fatalf("original run lost its authority after foreign rejection: effect=%+v err=%v", effect, err)
	}
}

func TestWorkflowTodosRejectMissingAndAlteredAuthorityWithoutAppend(t *testing.T) {
	for _, mutation := range []string{"missing_ticket", "invocation", "content_bytes", "node", "frozen_hash", "backend", "tool", "foreign_ticket_same_descriptor"} {
		t.Run(mutation, func(t *testing.T) {
			w, counter, call, frozen := pendingTodoOwner(t)
			validator := &todoTicketValidator{owner: w}
			request := claimTodoRequest(t, w, call, frozen, validator)
			switch mutation {
			case "missing_ticket":
				request.Authorization.Ticket = agent.ExecutionTicketRef{}
			case "invocation":
				request.InvocationID = "foreign-invocation"
			case "content_bytes":
				request.Content = append(request.Content, ' ')
			case "node":
				request.Authorization.Frozen.NodeExecutionID = "foreign-node"
			case "frozen_hash":
				request.Authorization.Frozen.Hash = "foreign-frozen-hash"
			case "backend":
				request.Authorization.Frozen.BackendID = "trusted-run"
			case "tool":
				request.Authorization.Frozen.Tool = "echo"
			case "foreign_ticket_same_descriptor":
				other, _, otherCall, otherFrozen := pendingTodoOwner(t)
				foreign := claimTodoRequest(t, other, otherCall, otherFrozen, other)
				request.Authorization.Ticket = foreign.Authorization.Ticket
			}
			before, appends := w.state.clone(), counter.appends
			effect, err := w.opts.Operations.Todos.Update(t.Context(), request)
			requireTodoCode(t, err, product.CodePermissionDenied)
			if effect.Confirmed || validator.calls.Load() != 0 || counter.appends != appends || !reflect.DeepEqual(before, w.state) || w.broken != nil {
				t.Fatal("invalid TODO request changed state, appended, or reached live validator")
			}
		})
	}
}

type foreignTodoClaimSink struct{}

func (foreignTodoClaimSink) CommitFact(context.Context, agent.ExecutionScope, agent.Fact) error {
	return nil
}

func TestWorkflowTodosExternalTicketCannotReplaceOwnDurableClaim(t *testing.T) {
	w, counter, call, frozen := pendingTodoOwner(t)
	// A separately trusted sink can produce its own ledger receipt, but that
	// receipt never claimed this run's original call in this run's journal.
	ledger := agent.NewBudget(w.opts.Limits)
	receipt, err := ledger.ClaimTool(t.Context(), foreignTodoClaimSink{}, call.Scope, call.Call, frozen)
	if err != nil {
		t.Fatal(err)
	}
	tickets, err := agent.NewExecutionTickets()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := tickets.Issue(receipt, frozen, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	request := agent.AuthorizedTodo{Authorization: agent.NewAuthorizedExecution(ref, frozen), InvocationID: frozen.Scope.InvocationID, Content: append(json.RawMessage(nil), frozen.FinalArguments...)}
	before := w.state.clone()
	effect, err := w.opts.Operations.Todos.Update(t.Context(), request)
	requireTodoCode(t, err, product.CodePermissionDenied)
	if effect.Confirmed || counter.appends != 0 || !reflect.DeepEqual(before, w.state) || w.state.Calls[call.Call.CallID].Claimed || w.state.Usage.ToolExecutions != 0 {
		t.Fatal("external ticket bypassed the run-owned durable claim")
	}
}

func TestWorkflowTodosRecheckPolicyAndCancellationBeforeAppend(t *testing.T) {
	for _, mutation := range []string{"current_policy", "no_longer_running", "cancelled_before_validate", "cancelled_after_validate", "active_cancelled"} {
		t.Run(mutation, func(t *testing.T) {
			w, counter, call, frozen := pendingTodoOwner(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			validator := &todoTicketValidator{owner: w}
			validator.after = func(context.Context) {
				w.mu.Lock()
				defer w.mu.Unlock()
				switch mutation {
				case "current_policy":
					w.state.Initial.Policy.Ref = "changed-policy"
				case "no_longer_running":
					w.state.Run.State = "pausing"
				case "cancelled_after_validate":
					cancel()
				case "active_cancelled":
					w.active.cancel()
				}
			}
			request := claimTodoRequest(t, w, call, frozen, validator)
			if mutation == "cancelled_before_validate" {
				cancel()
			}
			before := counter.appends
			effect, err := w.opts.Operations.Todos.Update(ctx, request)
			code := product.CodeStateConflict
			if mutation == "current_policy" {
				code = product.CodePermissionDenied
			}
			requireTodoCode(t, err, code)
			wantValidation := int32(1)
			if mutation == "cancelled_before_validate" {
				wantValidation = 0
			}
			if effect.Confirmed || counter.appends != before || len(w.state.TodoUpdates) != 0 || w.broken != nil || validator.calls.Load() != wantValidation {
				t.Fatal("precommit rejection appended, wrote, faulted, or validated repeatedly")
			}
			if (mutation == "cancelled_before_validate" || mutation == "cancelled_after_validate" || mutation == "active_cancelled") && !errors.Is(err, context.Canceled) {
				t.Fatal("precommit cancellation lost errors.Is proof")
			}
		})
	}
}
