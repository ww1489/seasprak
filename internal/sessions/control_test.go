package sessions

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func controlSession(t *testing.T, sid string, steps ...testkit.Step) (*AgentSession, *state.Manager, *testkit.FakeModel) {
	t.Helper()
	backend, err := memory.Open(sid, store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, sid)
	if err != nil {
		t.Fatal(err)
	}
	model := testkit.NewFake(steps...)
	s, err := Start(Options{SessionID: sid, Profile: ProfileMemory, Store: backend, Model: model, Principal: "local"}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, manager, model
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCancelTraceDurableReceiptAndReplay(t *testing.T) {
	gate := make(chan struct{})
	s, manager, model := controlSession(t, "cancel-op", testkit.Step{Gate: gate})
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"block"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return model.Calls() == 1 })
	receipt, err := s.CancelTrace(t.Context(), CancelTraceRequest{TraceID: in.TraceID, IdempotencyKey: "c1"})
	if err != nil || receipt.OperationID == "" || receipt.State != "accepted" {
		t.Fatalf("cancel not accepted: %v %+v", err, receipt)
	}
	// Acceptance is not stop: the trace is cancelling until the worker exits.
	waitFor(t, func() bool { return manager.View().Traces[in.TraceID].State == "cancelled" })
	waitFor(t, func() bool { return manager.View().Operations[receipt.OperationID].State == "completed" })
	op := manager.View().Operations[receipt.OperationID]
	if op.ResultRef != "cancelled" || model.Calls() != 1 {
		t.Fatalf("operation result=%q calls=%d", op.ResultRef, model.Calls())
	}
	again, err := s.CancelTrace(t.Context(), CancelTraceRequest{TraceID: in.TraceID, IdempotencyKey: "c1"})
	if err != nil || again != receipt {
		t.Fatal("replayed key did not return the original receipt")
	}
	if _, err = s.CancelTrace(t.Context(), CancelTraceRequest{TraceID: "other", IdempotencyKey: "c1"}); err == nil {
		t.Fatal("same key with different target accepted")
	} else if pe, _ := product.AsError(err); pe.Code != product.CodeIdempotencyConflict {
		t.Fatalf("code=%s", pe.Code)
	}
	terminal, err := s.CancelTrace(t.Context(), CancelTraceRequest{TraceID: in.TraceID, IdempotencyKey: "c2"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return manager.View().Operations[terminal.OperationID].State == "completed" })
	if manager.View().Operations[terminal.OperationID].ResultRef != "cancelled" || model.Calls() != 1 {
		t.Fatal("terminal cancel executed again")
	}
}

func TestCancelTraceRevisionConflictCommitsNothing(t *testing.T) {
	s, manager, _ := controlSession(t, "cancel-rev")
	stale := uint64(999)
	before := manager.View().LastSeq
	if _, err := s.CancelTrace(t.Context(), CancelTraceRequest{TraceID: "none", IdempotencyKey: "k", ExpectedRevision: &stale}); err == nil {
		t.Fatal("missing trace accepted")
	}
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return manager.View().Traces[in.TraceID].Settled })
	before = manager.View().LastSeq
	if _, err = s.CancelTrace(t.Context(), CancelTraceRequest{TraceID: in.TraceID, IdempotencyKey: "k", ExpectedRevision: &stale}); err == nil {
		t.Fatal("stale revision accepted")
	} else if pe, _ := product.AsError(err); pe.Code != product.CodeStateConflict {
		t.Fatalf("code=%s", pe.Code)
	}
	if manager.View().LastSeq != before || len(manager.View().Operations) != 0 {
		t.Fatal("rejected cancel committed")
	}
}

func TestContinueQueuedAtomicAndIdempotent(t *testing.T) {
	gate := make(chan struct{})
	s, manager, model := controlSession(t, "continue-op", testkit.Step{Gate: gate})
	first, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"first"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return model.Calls() == 1 })
	second, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"second"}`)})
	if err != nil {
		t.Fatal(err)
	}
	// Cancelling the active trace holds the independently queued one.
	stop, err := s.CancelTrace(t.Context(), CancelTraceRequest{TraceID: first.TraceID, IdempotencyKey: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return manager.View().Traces[first.TraceID].State == "cancelled" })
	// Cancellation operation completion is a separate durable commit. Wait
	// for it before measuring writes made by the rejected queue command.
	waitFor(t, func() bool { return manager.View().Operations[stop.OperationID].State == "completed" })
	if !manager.View().Traces[second.TraceID].Hold {
		t.Fatal("queued trace was not held")
	}
	beforeView := manager.View()
	before := beforeView.LastSeq
	if _, err = s.ContinueQueued(t.Context(), ContinueQueueRequest{TraceIDs: []string{second.TraceID, first.TraceID}, IdempotencyKey: "bad"}); err == nil {
		t.Fatal("batch with a non-queued trace accepted")
	}
	afterView := manager.View()
	if afterView.LastSeq != before || !afterView.Traces[second.TraceID].Hold || model.Calls() != 1 {
		t.Fatalf("partially invalid batch changed state: before=%d after=%d held=%t calls=%d cancel_before=%s cancel_after=%s", before, afterView.LastSeq, afterView.Traces[second.TraceID].Hold, model.Calls(), beforeView.Operations[stop.OperationID].State, afterView.Operations[stop.OperationID].State)
	}
	receipt, err := s.ContinueQueued(t.Context(), ContinueQueueRequest{TraceIDs: []string{second.TraceID}, IdempotencyKey: "go"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return manager.View().Traces[second.TraceID].Settled })
	again, err := s.ContinueQueued(t.Context(), ContinueQueueRequest{TraceIDs: []string{second.TraceID}, IdempotencyKey: "go"})
	if err != nil || again != receipt || model.Calls() != 2 {
		t.Fatalf("replay changed result: %v calls=%d", err, model.Calls())
	}
	if manager.View().Operations[receipt.OperationID].State != "completed" {
		t.Fatal("continue operation not completed")
	}
}

func TestCancelOperationSettledAfterReopen(t *testing.T) {
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "settle-reopen", Profile: ProfileMemory, Model: testkit.NewFake(), Principal: "local", GenerationFingerprint: "control-v1"}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	manager := s.rt.manager
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return manager.View().Traces[in.TraceID].Settled })
	// Simulate a crash between the terminal trace commit and operation settlement.
	receipt, _, err := manager.AcceptTraceControl(t.Context(), state.OperationCommand{Principal: "local", Kind: opCancelTrace, Target: in.TraceID, IdempotencyKey: "late"}, false, in.TraceID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err = OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	status, err := s.GetOperation(t.Context(), receipt.OperationID)
	if err != nil || status.State != "completed" || status.ResultRef != "completed" {
		t.Fatalf("operation not settled on reopen: %v %+v", err, status)
	}
}
