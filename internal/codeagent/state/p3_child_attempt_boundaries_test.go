package state_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

type childAttemptCountingStore struct {
	store.Store
	appends atomic.Int64
}

func (s *childAttemptCountingStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	s.appends.Add(1)
	return s.Store.Append(ctx, id, expected, c)
}

func TestP3ChildAttemptConcurrentTerminalsHaveOneAppend(t *testing.T) {
	_, backend, initial, msg, calls := childAttemptFixture(t)
	counter := &childAttemptCountingStore{Store: backend}
	m, err := state.NewManager(counter, "session")
	if err != nil {
		t.Fatal(err)
	}
	before := m.View()
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		results <- m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "accepted", "tool_calls", &msg, calls)
	}()
	go func() {
		<-start
		results <- m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "aborted", "", nil, nil)
	}()
	close(start)
	first, second := <-results, <-results
	if first != nil {
		first, second = second, first
	}
	if first != nil {
		t.Fatal("neither terminal committed", first, second)
	}
	requireP2Code(t, second, product.CodeStateConflict)
	after := m.View()
	if counter.appends.Load() != 1 || after.LastSeq != before.LastSeq+1 || len(after.AttemptResults) != 1 || after.ModelAttempts[initial.ID] != initial {
		t.Fatal("concurrent results appended or transitioned more than once")
	}
	requireChildParentUnchanged(t, before, after)
	if after.AttemptResults[initial.ID].State == "accepted" {
		if !reflect.DeepEqual(after.Invocations[initial.Scope.InvocationID].MessageIDs, []string{initial.MessageID}) || len(after.Calls) != len(before.Calls)+1 {
			t.Fatal("winning accepted terminal was partial")
		}
	} else if len(after.InvocationMessages) != 0 || !reflect.DeepEqual(before.Calls, after.Calls) || !reflect.DeepEqual(before.Invocations, after.Invocations) {
		t.Fatal("losing candidate became visible after abort")
	}
}

func TestP3ChildAttemptValidationAndCallerCancellationHaveNoAppend(t *testing.T) {
	_, backend, initial, msg, calls := childAttemptFixture(t)
	counter := &childAttemptCountingStore{Store: backend}
	m, err := state.NewManager(counter, "session")
	if err != nil {
		t.Fatal(err)
	}
	before := m.View()
	requireP2Code(t, m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "accepted", "", nil, nil), product.CodeInvalidArgument)
	bad := childAttemptDetails(initial)
	bad.Usage[0].Request.ModelCallID = "parent-turn"
	requireP2Code(t, m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "accepted", "", &msg, calls, bad), product.CodeStateConflict)
	requireP2Code(t, m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "accepted", "", &msg, calls, childAttemptDetails(initial), childAttemptDetails(initial)), product.CodeInvalidArgument)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.SaveAttemptResult(ctx, initial.Scope, initial.ID, "accepted", "", &msg, calls); err != context.Canceled {
		t.Fatal("caller cancellation was not preserved", err)
	}
	if counter.appends.Load() != 0 || !reflect.DeepEqual(before, m.View()) || m.Fault() != nil {
		t.Fatal("rejected candidate reached append or faulted manager")
	}
}

func TestP3ChildAttemptNestedInvocationAcceptsItsOwnCandidate(t *testing.T) {
	m, backend, initial, msg, calls := childAttemptFixture(t)
	calls[0].Call.Name, calls[0].Call.Arguments = "delegate_task", `{"agent":"worker","task":"nested"}`
	msg.Standard.ContentBlocks[0].FunctionToolCall.Name, msg.Standard.ContentBlocks[0].FunctionToolCall.Arguments = calls[0].Call.Name, calls[0].Call.Arguments
	before := m.View()
	if err := m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "accepted", "tool_calls", &msg, calls); err != nil {
		t.Fatal(err)
	}
	call := calls[0]
	call.Claimed = true
	if err := m.SaveCall(t.Context(), call); err != nil {
		t.Fatal(err)
	}
	inv := state.Invocation{ID: "nested-child", ParentInvocationID: initial.Scope.InvocationID, ParentCallID: call.Call.CallID, TraceID: initial.Scope.TraceID, State: "running", Target: agent.TargetAgent{Name: "worker", Version: "v1", Generation: "g"}}
	if err := m.SaveInvocation(t.Context(), inv); err != nil {
		t.Fatal(err)
	}
	nested := initial
	nested.ID, nested.ModelCallID, nested.MessageID, nested.StreamID = "nested-attempt", "nested-model-call", "nested-candidate", "nested-stream"
	nested.Scope.InvocationID, nested.Scope.ParentInvocationID = inv.ID, inv.ParentInvocationID
	if err := m.SaveRecords(t.Context(), m.View().LastSeq, state.Records{ModelAttempts: []state.ModelAttempt{nested}}); err != nil {
		t.Fatal(err)
	}
	msg.ID, msg.Scope.InvocationID, msg.Standard.ContentBlocks = nested.MessageID, nested.Scope.InvocationID, nil
	if err := m.SaveAttemptResult(t.Context(), nested.Scope, nested.ID, "accepted", "stop", &msg, nil); err != nil {
		t.Fatal(err)
	}
	after := m.View()
	requireChildParentUnchanged(t, before, after)
	if len(after.InvocationMessages) != 2 || !reflect.DeepEqual(after.Invocations[inv.ID].MessageIDs, []string{nested.MessageID}) || !reflect.DeepEqual(after.Invocations[initial.Scope.InvocationID].MessageIDs, []string{initial.MessageID}) {
		t.Fatal("nested candidate entered another invocation's projection")
	}
	reopened, err := state.NewManager(backend, "session")
	if err != nil || !reflect.DeepEqual(after, reopened.View()) {
		t.Fatal("nested acceptance replay differs", err)
	}
}

func TestP3ChildAttemptLegacyFailedJournalDoesNotAcquirePrivateProjection(t *testing.T) {
	m, backend, initial, msg, _ := childAttemptFixture(t)
	msg.Status = agent.StatusIncomplete
	msg.Standard.ContentBlocks = nil
	leaf := m.View().LeafID
	if err := m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "failed", "", &msg, nil); err != nil {
		t.Fatal(err)
	}
	legacy := childResumeMutationStore{Store: backend, mutate: func(c *store.Commit) {
		var controls []store.Record
		for _, r := range c.ControlRecords {
			if r.Type == "invocation_message" {
				r.Type, r.ParentID = "message", leaf
				c.Entries = []store.Record{r}
			} else {
				controls = append(controls, r)
			}
		}
		c.ControlRecords = controls
		c.Events[0].Type = "model.attempt_failed"
		c.Events[0].Payload, _ = json.Marshal(m.View().AttemptResults[initial.ID])
	}}
	reopened, err := state.NewManager(legacy, "session")
	if err != nil {
		t.Fatal("legacy failure journal could not load", err)
	}
	view := reopened.View()
	if len(view.InvocationMessages) != 0 || len(view.Invocations[initial.Scope.InvocationID].MessageIDs) != 0 || len(view.AttemptResults) != 1 || view.AttemptResults[initial.ID].State != "failed" || len(view.Messages) != len(m.View().Messages)+1 {
		t.Fatal("legacy journal gained a new child acceptance projection")
	}
}

func TestP3ChildAttemptReplayRejectsShrinkingOrReorderedOwnedMessages(t *testing.T) {
	for _, kind := range []string{"shrink", "replace", "terminal-duplicate"} {
		t.Run(kind, func(t *testing.T) {
			m, backend, initial, msg, calls := childAttemptFixture(t)
			if err := m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "accepted", "", &msg, calls); err != nil {
				t.Fatal(err)
			}
			inv, _ := m.Invocation(initial.Scope.InvocationID)
			inv.State = "completed"
			if err := m.SaveInvocation(t.Context(), inv); err != nil {
				t.Fatal(err)
			}
			corrupt := childResumeMutationStore{Store: backend, mutate: func(c *store.Commit) {
				var next state.Invocation
				r := c.ControlRecords[0]
				_ = json.Unmarshal(r.Payload, &next)
				switch kind {
				case "shrink":
					next.MessageIDs = nil
				case "replace":
					next.MessageIDs[0] = "forged"
				case "terminal-duplicate":
					c.ControlRecords = append(c.ControlRecords, r)
				}
				r.Payload, _ = json.Marshal(next)
				c.ControlRecords[0] = r
			}}
			if _, err := state.NewManager(corrupt, "session"); err == nil {
				t.Fatal("accepted child message list or terminal invocation was mutable on replay")
			}
		})
	}
}
