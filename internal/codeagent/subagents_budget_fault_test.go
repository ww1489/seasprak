package codeagent

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type p3ChildReservationStore struct {
	store.Store
	physical    bool
	fail        bool
	reached     atomic.Bool
	afterCommit func()
}

func (s *p3ChildReservationStore) Append(ctx context.Context, sid string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	selected := false
	for _, r := range commit.ControlRecords {
		if r.Type != "invocation_budget" {
			continue
		}
		var budget struct {
			Usage agent.Usage `json:"usage"`
		}
		if json.Unmarshal(r.Payload, &budget) == nil && (budget.Usage.TransportRequests > 0) == s.physical {
			selected = true
		}
	}
	if selected && s.fail {
		s.reached.Store(true)
		return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "synthetic child reservation append failure")
	}
	receipt, err := s.Store.Append(ctx, sid, expected, commit)
	if selected && err == nil && s.reached.CompareAndSwap(false, true) && s.afterCommit != nil {
		s.afterCommit()
	}
	return receipt, err
}

func TestP3ChildBudgetAppendFailureStartsNoRequest(t *testing.T) {
	for _, physical := range []bool{false, true} {
		name := "logical"
		if physical {
			name = "physical"
		}
		t.Run(name, func(t *testing.T) {
			var effects atomic.Int32
			child := newP3ChildAttemptModel(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "forbidden", Name: "probe", Arguments: `{}`}}})
			main := testkit.NewFake(delegateCall("worker", "own task"), testkit.Step{Text: "parent must not continue"})
			opts := subagentOptions(agentRoots(t), "child-budget-fault-"+name, main,
				[]agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"probe"}}}, []tools.Definition{countedTool("probe", &effects, nil)})
			backend, err := memory.Open(opts.SessionID, store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			faults := &p3ChildReservationStore{Store: backend, physical: physical, fail: true}
			opts.Store = faults
			s := openSubagentSession(t, opts, true)
			child.manager = s.rt.manager
			in := submitPrompt(t, s, "delegate")
			waitFor(t, func() bool { return faults.reached.Load() || s.rt.manager.View().Traces[in.TraceID].Settled })
			if !faults.reached.Load() {
				t.Fatal("child budget persistence was never invoked")
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			v := s.rt.manager.View()
			inv := onlyInvocation(t, v)
			wantModels, wantLogical, wantAttempts := int32(0), 0, 0
			if physical {
				wantModels, wantLogical, wantAttempts = 1, 1, 1
			}
			budget := v.InvocationBudgets[inv.ID]
			if child.generates.Load() != wantModels || child.physical.Load() != 0 || effects.Load() != 0 || child.FakeModel.Calls() != 0 || main.Calls() != 1 || len(inv.CallIDs) != 0 || len(inv.MessageIDs) != 0 || len(childAttemptRecords(v, inv.ID)) != wantAttempts || budget.Usage.LogicalModelCalls != wantLogical || budget.Usage.TransportRequests != 0 || v.Traces[in.TraceID].Usage.LogicalModelCalls != 1+wantLogical || v.Traces[in.TraceID].Usage.TransportRequests != 1 {
				t.Fatal("failed child reservation admitted wire/tool work, published a candidate, or charged only one ledger")
			}
		})
	}
}

func TestP3ChildBudgetCancellationAfterCommitKeepsReservationWithoutWire(t *testing.T) {
	child := newP3ChildAttemptModel(testkit.Step{Text: "never sent"})
	main := testkit.NewFake(delegateCall("worker", "own task"), testkit.Step{Text: "parent must not continue"})
	opts := subagentOptions(agentRoots(t), "child-budget-commit-cancel", main,
		[]agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true}}, nil)
	seed := openSubagentSession(t, opts, true)
	opts, generation := seed.rt.opts, seed.rt.generation
	if err := seed.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	backend, err := jsonl.Open(opts.SessionID, opts.StateRoot, store.Header{}, jsonl.Options{OpenExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	reserved, release := make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	unblock := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	defer unblock()
	faults := &p3ChildReservationStore{Store: backend, physical: true}
	opts.Store = faults
	manager, err := state.NewManager(faults, opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Start(opts, manager, generation)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	t.Cleanup(unblock)
	child.manager = s.rt.manager
	// Append is invoked by the mailbox-owned commit. Inject cancellation only
	// after the real store has accepted both reservations, then delay receipt.
	// This is a failure-window test, not a public Cancel/Pause certification.
	faults.afterCommit = func() { s.rt.active.cancel(); close(reserved); <-release }
	in := submitPrompt(t, s, "delegate")
	waitCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-reserved:
	case <-waitCtx.Done():
		t.Fatal("reservation did not commit")
	}
	unblock()
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	v := s.rt.manager.View()
	inv := onlyInvocation(t, v)
	budget := v.InvocationBudgets[inv.ID]
	attempts := childAttemptRecords(v, inv.ID)
	if child.generates.Load() != 1 || child.physical.Load() != 0 || child.FakeModel.Calls() != 0 || main.Calls() != 1 || len(attempts) != 1 || budget.Usage.LogicalModelCalls != 1 || budget.Usage.TransportRequests != 1 || budget.Usage.ModelRequests != 1 || budget.Usage.LastTransport.AttemptID != attempts[0].ID || v.Traces[in.TraceID].Usage.LogicalModelCalls != 2 || v.Traces[in.TraceID].Usage.TransportRequests != 2 {
		t.Fatal("post-commit cancellation sent a request or refunded durable occupancy")
	}
	if result := v.AttemptResults[attempts[0].ID]; result.State != "aborted" || len(v.AttemptDetails[result.DiagnosticRef].Usage) != 0 {
		t.Fatal("an unsent reservation became successful or fabricated actual usage")
	}
	s = openSubagentSession(t, opts, false)
	after := s.rt.manager.View()
	if !reflect.DeepEqual(after.InvocationBudgets[inv.ID], budget) || after.Traces[in.TraceID].Usage != v.Traces[in.TraceID].Usage || child.physical.Load() != 0 || main.Calls() != 1 {
		t.Fatal("reopen executed an unsent reservation or refunded it")
	}
}
