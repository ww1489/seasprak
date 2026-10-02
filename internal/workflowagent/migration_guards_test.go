package workflowagent

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
)

type migrationAppendCounter struct {
	storage.Store
	appends int
}

func (s *migrationAppendCounter) Append(ctx context.Context, id string, e storage.ExpectedCommit, c storage.Commit) (storage.CommitReceipt, error) {
	s.appends++
	return s.Store.Append(ctx, id, e, c)
}

// Build a valid running prefix through the real factory and tool pipeline.
// With no grant, fail the intent append before any effect; with a grant, the
// real runtime pauses for approval. Replay only through the frozen descriptor.
func migrationPendingTool(t *testing.T, grant string, effects *atomic.Int32) (*WorkflowAgent, *migrationAppendCounter, agent.ToolRecord, agent.FrozenExecution) {
	t.Helper()
	opts := testOptions(t, toolOnly(), nil, effects)
	opts.Tools[0].Execution.RequestedGrantRef = grant
	seed := injectStore(t, &opts)
	if grant == "" {
		seed.reject = func(c storage.Commit) error {
			for _, r := range c.ControlRecords {
				if r.Type == "workflow_tool_intent" {
					return product.NewError(product.CodeStorageUnavailable, "fixture stops before tool claim")
				}
			}
			return nil
		}
	}
	w := newWorkflow(t, opts)
	submit(t, w)
	waitExited(t, w)
	loaded, err := seed.Load(t.Context(), opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
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
	if !found || effects.Load() != 0 {
		t.Fatal("fixture lacks a frozen, unexecuted tool")
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
	probe.active = &segment{scope: call.Scope, ctx: t.Context()}
	return probe, counter, call, frozen
}

func migrationCommit(t *testing.T, w *WorkflowAgent, records ...storage.Record) error {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.commitLocked(t.Context(), records, nil)
}

func migrationGuardUnchanged(t *testing.T, w *WorkflowAgent, counter *migrationAppendCounter, before runState, appends int, effects *atomic.Int32) {
	t.Helper()
	if !reflect.DeepEqual(before, w.state) || w.state.Revision != before.Revision || counter.appends != appends || effects.Load() != 0 || w.broken != nil {
		t.Fatal("rejected guard changed state/revision, appended, executed, or faulted")
	}
}

func TestWorkflowMigrationDuplicateNodeBeginWritesNothing(t *testing.T) {
	var effects atomic.Int32
	w, counter, call, _ := migrationPendingTool(t, "once", &effects)
	before := w.state.clone()
	err := migrationCommit(t, w, nodeFact(w.state.Nodes[call.Scope.NodeExecutionID]))
	requireCode(t, err, product.CodeIncompatibleVersion)
	migrationGuardUnchanged(t, w, counter, before, 0, &effects)
}

func TestWorkflowMigrationTerminalNodeMutationWritesNothing(t *testing.T) {
	for _, terminalState := range []string{"completed", "failed"} {
		t.Run(terminalState, func(t *testing.T) {
			var effects atomic.Int32
			w, counter, call, _ := migrationPendingTool(t, "", &effects)
			n := w.state.Nodes[call.Scope.NodeExecutionID]
			if terminalState == "completed" {
				budget := n.Usage
				budget.ToolExecutions++
				raw, _ := json.Marshal(call.Call)
				// Positive control: an ordinary no-approval claim is legal.
				if err := w.CommitFact(t.Context(), call.Scope, agent.Fact{Kind: "tool_intent", Payload: raw, Budget: &budget}); err != nil {
					t.Fatal(err)
				}
				call = w.state.Calls[call.Call.CallID]
				call.Observation = &agent.ToolObservation{Status: "succeeded", SideEffect: "none", Content: "original"}
				n = w.state.Nodes[n.ID]
				n.Result = "original"
			} else {
				call.Observation = &agent.ToolObservation{Status: "failed", SideEffect: "none"}
				n.ErrorCode = product.CodeResourceUnavailable
			}
			n.State = terminalState
			if err := migrationCommit(t, w, record("workflow_tool_observation", call.Call.CallID, call), nodeFact(n)); err != nil {
				t.Fatal(err)
			}
			for _, next := range []string{"accepted", "waiting", "completed", "failed"} {
				t.Run(next, func(t *testing.T) {
					before, appends := w.state.clone(), counter.appends
					forged := n
					forged.State = next
					err := migrationCommit(t, w, nodeFact(forged))
					requireCode(t, err, product.CodeIncompatibleVersion)
					migrationGuardUnchanged(t, w, counter, before, appends, &effects)
				})
			}
		})
	}
}

func TestWorkflowMigrationApprovalWaitRequiresFrozenGrant(t *testing.T) {
	for _, kind := range []string{"missing_frozen", "no_requested_grant"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			w, counter, call, frozen := migrationPendingTool(t, "", &effects)
			if kind == "missing_frozen" {
				delete(w.state.Frozen, frozen.ID)
			}
			before := w.state.clone()
			wait, err := w.RequestToolApproval(t.Context(), call.Scope, frozen)
			if kind == "missing_frozen" {
				requireCode(t, err, product.CodePermissionDenied)
			} else {
				requireCode(t, err, product.CodeStateConflict)
			}
			if wait != nil || len(w.approvals) != 0 {
				t.Fatal("approval was requested without its frozen grant")
			}
			migrationGuardUnchanged(t, w, counter, before, 0, &effects)
			// Direct state commits cannot manufacture the waiting state either.
			n := w.state.Nodes[call.Scope.NodeExecutionID]
			n.State = "waiting"
			err = migrationCommit(t, w, nodeFact(n))
			requireCode(t, err, product.CodeIncompatibleVersion)
			migrationGuardUnchanged(t, w, counter, before, 0, &effects)
		})
	}
}

func TestWorkflowMigrationApprovalClaimRequiresCurrentPermission(t *testing.T) {
	for _, kind := range []string{"never_waited", "waiting_without_answer"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			w, counter, call, frozen := migrationPendingTool(t, "once", &effects)
			if kind == "waiting_without_answer" {
				if _, err := w.RequestToolApproval(t.Context(), call.Scope, frozen); err != nil {
					t.Fatal(err)
				}
				n := w.state.Nodes[call.Scope.NodeExecutionID]
				n.State = "waiting"
				if err := migrationCommit(t, w, nodeFact(n)); err != nil {
					t.Fatal(err)
				}
				// A new segment re-admits the original node, but no response has
				// supplied permission. Keep the instance's pending question.
				n.State = "accepted"
				if err := migrationCommit(t, w, nodeFact(n)); err != nil {
					t.Fatal(err)
				}
			}
			before, appends := w.state.clone(), counter.appends
			budget := w.state.Nodes[call.Scope.NodeExecutionID].Usage
			budget.ToolExecutions++
			raw, _ := json.Marshal(call.Call)
			err := w.CommitFact(t.Context(), call.Scope, agent.Fact{Kind: "tool_intent", Payload: raw, Budget: &budget})
			requireCode(t, err, product.CodePermissionDenied)
			migrationGuardUnchanged(t, w, counter, before, appends, &effects)
			if w.state.Calls[call.Call.CallID].Claimed {
				t.Fatal("unapproved original call was claimed")
			}
		})
	}
}
