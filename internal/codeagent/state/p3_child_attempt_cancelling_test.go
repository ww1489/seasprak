package state_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestP3ChildAttemptCancellingPersistsOnlyFailureTerminals(t *testing.T) {
	for _, status := range []string{"failed", "incomplete", "aborted"} {
		for _, candidate := range []bool{false, true} {
			t.Run(status+map[bool]string{false: "-no-candidate", true: "-candidate"}[candidate], func(t *testing.T) {
				m, backend, initial, msg, calls := childAttemptFixture(t)
				if err := m.SetTraceState(t.Context(), initial.Scope.TraceID, "cancelling", false); err != nil {
					t.Fatal(err)
				}
				counter := &childAttemptCountingStore{Store: backend}
				m, err := state.NewManager(counter, "session")
				if err != nil {
					t.Fatal(err)
				}
				before := m.View()
				msg.Status = agent.StatusIncomplete
				var private *agent.AgentMessage
				if candidate {
					private = &msg
				}
				details := childAttemptDetails(initial)
				if err := m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, status, "", private, nil, details); err != nil {
					t.Fatalf("cancelling must preserve the existing child's %s terminal: %v", status, err)
				}
				after := m.View()
				result := after.AttemptResults[initial.ID]
				if counter.appends.Load() != 1 || after.LastSeq != before.LastSeq+1 || len(after.AttemptResults) != 1 || result.State != status || result.ExpectedRevision != 1 || result.Revision != 2 || after.ModelAttempts[initial.ID] != initial || len(after.UnfinishedAttempts) != 0 {
					t.Fatal("cancelling terminal changed the initial attempt or did not commit once")
				}
				if result.UsageRef != result.DiagnosticRef || !reflect.DeepEqual(after.AttemptDetails[result.UsageRef].ModelAttemptDetails, details) || !reflect.DeepEqual(before.Invocations, after.Invocations) || !reflect.DeepEqual(before.Calls, after.Calls) || !reflect.DeepEqual(before.Traces, after.Traces) || before.Budget != after.Budget {
					t.Fatal("cancelling terminal lost evidence, admitted work or changed budget/state")
				}
				if candidate && !reflect.DeepEqual(after.InvocationMessages[msg.ID], msg) || !candidate && len(after.InvocationMessages) != 0 {
					t.Fatal("cancelling terminal lost or fabricated the private diagnostic")
				}
				requireChildParentUnchanged(t, before, after)
				requireChildSafeEvents(t, after.Events[len(before.Events):], initial, status)
				requireP2Code(t, m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "aborted", "", nil, nil), product.CodeStateConflict)
				// Closing an existing attempt must not authorize new child work.
				requireP2Code(t, m.RegisterChildCalls(t.Context(), initial.Scope.InvocationID, calls), product.CodeStateConflict)
				inv := after.Invocations[initial.Scope.InvocationID]
				inv.ID = "new-child-during-cancelling"
				requireP2Code(t, m.SaveInvocation(t.Context(), inv), product.CodeStateConflict)
				if counter.appends.Load() != 1 || !reflect.DeepEqual(after, m.View()) {
					t.Fatal("duplicate terminal or new child work was admitted during cancelling")
				}
				reopened, err := state.NewManager(backend, "session")
				if err != nil || !reflect.DeepEqual(after, reopened.View()) {
					t.Fatal("load-only replay rejected or changed the cancelling terminal", err)
				}
			})
		}
	}
}

func TestP3ChildAttemptCancellingRetainsActiveIdentityAndStopGuards(t *testing.T) {
	for _, kind := range []string{"accepted", "exit-proof", "interrupted", "terminal", "parent-ended", "wrong-parent", "failure-tools"} {
		t.Run(kind, func(t *testing.T) {
			m, backend, initial, msg, calls := childAttemptFixture(t)
			if err := m.SetTraceState(t.Context(), initial.Scope.TraceID, "cancelling", false); err != nil {
				t.Fatal(err)
			}
			status, code := "aborted", product.CodeStateConflict
			msg.Status = agent.StatusIncomplete
			var candidateCalls []agent.ToolRecord
			scope := initial.Scope
			switch kind {
			case "accepted":
				status, msg.Status, candidateCalls = "accepted", agent.StatusComplete, calls
			case "exit-proof":
				if err := m.ConfirmExecutionStopped(t.Context(), initial.Scope.TraceID); err != nil {
					t.Fatal(err)
				}
			case "interrupted", "terminal":
				inv, _ := m.Invocation(initial.Scope.InvocationID)
				inv.State = "interrupted"
				if kind == "terminal" {
					inv.State = "cancelled"
				}
				if err := m.SaveInvocation(t.Context(), inv); err != nil {
					t.Fatal(err)
				}
			case "parent-ended":
				parent := m.View().Calls["delegate"]
				parent.Observation = &agent.ToolObservation{Status: "cancelled", SideEffect: "none"}
				if err := m.SaveCall(t.Context(), parent); err != nil {
					t.Fatal(err)
				}
			case "wrong-parent":
				scope.ParentInvocationID = "other-parent"
			case "failure-tools":
				candidateCalls, code = calls, product.CodeInvalidArgument
			}
			counter := &childAttemptCountingStore{Store: backend}
			m, err := state.NewManager(counter, "session")
			if err != nil {
				t.Fatal(err)
			}
			before := m.View()
			requireP2Code(t, m.SaveAttemptResult(t.Context(), scope, initial.ID, status, "", &msg, candidateCalls), code)
			if counter.appends.Load() != 0 || !reflect.DeepEqual(before, m.View()) {
				t.Fatal("cancelling admitted an accepted, stale, stopped or wrongly scoped result")
			}
		})
	}
}

func TestP3ChildAttemptCancellingAppendFailurePublishesNothing(t *testing.T) {
	for _, status := range []string{"failed", "incomplete", "aborted"} {
		t.Run(status, func(t *testing.T) {
			m, backend, initial, msg, _ := childAttemptFixture(t)
			if err := m.SetTraceState(t.Context(), initial.Scope.TraceID, "cancelling", false); err != nil {
				t.Fatal(err)
			}
			msg.Status = agent.StatusIncomplete
			before := m.View()
			backend.fail = true
			err := m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, status, "", &msg, nil, childAttemptDetails(initial))
			if err == nil || !strings.Contains(err.Error(), "injected sync failure") || !reflect.DeepEqual(before, m.View()) {
				t.Fatal("failed append published cancelling terminal or never reached append", err)
			}
			reopened, err := state.NewManager(backend, "session")
			if err != nil || !reflect.DeepEqual(before, reopened.View()) || len(reopened.View().UnfinishedAttempts) != 1 {
				t.Fatal("reopen fabricated a terminal after cancelled append failure", err)
			}
		})
	}
}
