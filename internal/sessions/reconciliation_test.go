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
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func makeReconcileSession(t *testing.T, query ReconcileQuery, stopped ...bool) (*AgentSession, *state.Manager, *tools.ResourceScheduler, agent.InputReceipt, string) {
	t.Helper()
	backend, err := memory.Open("reconcile-session", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "reconcile-session")
	if err != nil {
		t.Fatal(err)
	}
	input, err := manager.Accept(context.Background(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)}, agent.TargetAgent{Name: "main", Version: "main-v1", Generation: "gen"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetTraceState(context.Background(), input.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	tr := manager.View().Traces[input.TraceID]
	turn := agent.TurnRecord{ID: "turn", TraceID: input.TraceID, InvocationID: tr.InvocationID}
	if err := manager.SaveTurn(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	call := agent.ToolRecord{Call: agent.FrozenCall{CallID: "call", ProviderCallID: "provider", Name: "work", Arguments: `{}`, Generation: "gen"}, Scope: agent.ExecutionScope{SessionID: "reconcile-session", BranchID: "main", TraceID: input.TraceID, InvocationID: tr.InvocationID, Generation: "gen", TurnID: turn.ID}}
	message := agent.AgentMessage{ID: "assistant", Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: "reconcile-session", TraceID: input.TraceID, InvocationID: tr.InvocationID, TurnID: turn.ID}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider", Name: "work", Arguments: `{}`})}}}
	if err := manager.SaveAssistant(context.Background(), message, []agent.ToolRecord{call}); err != nil {
		t.Fatal(err)
	}
	call.Claimed = true
	call.Observation = &agent.ToolObservation{Status: "outcome_unknown", SideEffect: "unknown", Executed: true}
	if err := manager.SaveCall(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetTraceState(context.Background(), input.TraceID, "paused", false); err != nil {
		t.Fatal(err)
	}
	if len(stopped) == 0 || stopped[0] {
		if err := manager.ConfirmExecutionStopped(context.Background(), input.TraceID); err != nil {
			t.Fatal(err)
		}
	}
	scheduler := tools.NewResourceScheduler()
	queries := map[string]ReconcileQuery{}
	if query != nil {
		queries["no-start"] = query
	}
	s, err := Start(Options{SessionID: "reconcile-session", Workspace: t.TempDir(), Profile: ProfileMemory, Principal: "operator", Store: backend, Model: testkit.NewFake(testkit.Step{Text: "unused"}), ResourceScheduler: scheduler, ReconcileQueries: queries}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, manager, scheduler, input, call.Call.CallID
}

func TestReconcileTrustedNoStartDoesNotRerunAndReleasesOnlyItsHold(t *testing.T) {
	var calls atomic.Int32
	s, manager, scheduler, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(_ context.Context, request ReconcileQueryRequest) (ReconcileEvidence, error) {
		calls.Add(1)
		if request.CallID != "call" || request.TraceID == "" || request.ObservationID == "" || request.ObservationVersion == 0 || request.OriginalGrantRef != "" {
			t.Fatalf("unexpected trusted query request: %+v", request)
		}
		return ReconcileEvidence{EvidenceRefs: []string{"query-evidence"}, EvidenceSource: "test-query", TrustedNoStart: true}, nil
	}))
	before := manager.View()
	if !scheduler.HasHold("reconcile-session\x00" + callID) {
		t.Fatal("unknown call did not retain its scheduling hold")
	}
	receipt, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start", IdempotencyKey: "reconcile-once"})
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.GetOperation(t.Context(), receipt.OperationID)
	if err != nil || status.State != "completed" {
		t.Fatalf("reconcile operation=%+v err=%v", status, err)
	}
	view := manager.View()
	if calls.Load() != 1 || scheduler.HasHold("reconcile-session\x00"+callID) || view.HasUnresolvedEffects() || view.Calls[callID].Observation.SideEffect != "unknown" || view.Traces[input.TraceID].State != "paused" {
		t.Fatalf("trusted no-start changed execution or hold incorrectly: calls=%d hold=%v view=%+v", calls.Load(), scheduler.HasHold("reconcile-session\x00"+callID), view)
	}
	if len(view.Reconciliations) != 1 || view.Operations[receipt.OperationID].ResultRef == "" {
		t.Fatal("reconciliation result was not durably recorded")
	}
	if _, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: view.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start", IdempotencyKey: "reconcile-once"}); err != nil {
		t.Fatal("idempotent reconciliation retry failed: ", err)
	}
	if calls.Load() != 1 {
		t.Fatal("idempotent reconciliation reran the trusted query")
	}
}

func TestReconcileTrustedNoStartDoesNotRestoreReleasedHold(t *testing.T) {
	s, manager, _, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
		return ReconcileEvidence{EvidenceRefs: []string{"query-evidence"}, EvidenceSource: "test-query", TrustedNoStart: true}, nil
	}))
	before := manager.View()
	if _, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start"}); err != nil {
		t.Fatal(err)
	}
	restored := tools.NewResourceScheduler()
	reopened := &runtime{opts: Options{SessionID: "reconcile-session", Workspace: s.rt.opts.Workspace, ResourceEnvironment: s.rt.opts.ResourceEnvironment, ResourceScheduler: restored}, manager: manager}
	if err := reopened.restoreResourceHolds(); err != nil {
		t.Fatal(err)
	}
	if restored.HasHold(tools.ResourceHoldID("reconcile-session", callID)) || manager.View().HasUnresolvedEffects() {
		t.Fatal("trusted no-start was restored as an unresolved resource hold")
	}
}

func latestReconcileObservation(view state.View, callID string) state.ObservationRevision {
	var latest state.ObservationRevision
	for _, observation := range view.Observations {
		if observation.CallID == callID && observation.Version > latest.Version {
			latest = observation
		}
	}
	return latest
}

func TestReconcileRejectsMismatchedGrantReference(t *testing.T) {
	s, manager, _, input, callID := makeReconcileSession(t, nil)
	before := manager.View()
	_, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, EvidenceRef: "manual-evidence", GrantRef: "unexpected-grant"})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict || len(manager.View().Operations) != 0 {
		t.Fatalf("mismatched grant was accepted: err=%v view=%+v", err, manager.View())
	}
}

func TestReconcileEvidenceReferenceCannotClearUnknownOrReleaseHold(t *testing.T) {
	s, manager, scheduler, input, callID := makeReconcileSession(t, nil)
	before := manager.View()
	receipt, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, EvidenceRef: "manual-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.GetOperation(t.Context(), receipt.OperationID)
	if err != nil || status.State != "completed" || status.Result == nil || status.Result.EvidenceSource != "evidence:manual-evidence" {
		t.Fatalf("manual evidence result=%+v err=%v", status, err)
	}
	view := manager.View()
	latest := latestReconcileObservation(view, callID)
	if latest.Observation.SideEffect != "unknown" || latest.Observation.Status != "outcome_unknown" || !view.HasUnresolvedEffects() || !scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)) {
		t.Fatalf("manual evidence cleared an unresolved effect: latest=%+v hold=%v view=%+v", latest, scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)), view)
	}
}

func TestReconcileQueryFailureIsDurablyRecordedWithoutChangingEvidence(t *testing.T) {
	var calls atomic.Int32
	s, manager, scheduler, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
		calls.Add(1)
		return ReconcileEvidence{}, errors.New("query backend failed")
	}))
	before := manager.View()
	receipt, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start"})
	if err == nil || receipt.OperationID == "" {
		t.Fatalf("query failure was not returned with an accepted receipt: receipt=%+v err=%v", receipt, err)
	}
	status, statusErr := s.GetOperation(t.Context(), receipt.OperationID)
	after := manager.View()
	if statusErr != nil || status.State != "failed" || status.ErrorRef != "reconciliation query failed" || len(after.Observations) != len(before.Observations) || calls.Load() != 1 || !scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)) {
		t.Fatalf("query failure was not durably isolated: status=%+v statusErr=%v calls=%d before=%+v after=%+v", status, statusErr, calls.Load(), before, after)
	}
}

func TestReconcileConfirmedQueryKeepsConsumedResourceHold(t *testing.T) {
	s, manager, scheduler, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
		return ReconcileEvidence{EvidenceRefs: []string{"confirmed-evidence"}, EvidenceSource: "test-query", ConfirmedExecution: true, ConfirmedEffects: []string{"effect"}}, nil
	}))
	before := manager.View()
	receipt, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start"})
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.GetOperation(t.Context(), receipt.OperationID)
	view := manager.View()
	if err != nil || status.Result == nil || len(status.Result.ConfirmedEffects) != 1 || view.HasUnresolvedEffects() || !scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)) || view.Calls[callID].Observation.SideEffect != "unknown" {
		t.Fatalf("confirmed reconciliation lost consumed facts: status=%+v err=%v hold=%v view=%+v", status, err, scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)), view)
	}
}

func TestReconcileCannotFinalizeWhileExecutionMayStillRun(t *testing.T) {
	s, manager, scheduler, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
		return ReconcileEvidence{EvidenceRefs: []string{"query-evidence"}, EvidenceSource: "test-query", TrustedNoStart: true}, nil
	}), false)
	before := manager.View()
	receipt, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start"})
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.GetOperation(t.Context(), receipt.OperationID)
	view := manager.View()
	latest := latestReconcileObservation(view, callID)
	if err != nil || status.Result == nil || latest.Observation.SideEffect != "unknown" || !view.HasUnresolvedEffects() || !scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)) || len(status.Result.ConflictRestrictions) == 0 {
		t.Fatalf("reconciliation finalized while execution was not stopped: status=%+v err=%v latest=%+v hold=%v view=%+v", status, err, latest, scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)), view)
	}
}

func TestReconcileRepeatedUnknownEvidenceUsesNewObservationIDs(t *testing.T) {
	var calls atomic.Int32
	s, manager, _, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
		calls.Add(1)
		return ReconcileEvidence{EvidenceRefs: []string{"unknown-evidence"}, EvidenceSource: "test-query", RemainingUnknown: []string{"still unknown"}}, nil
	}))
	before := manager.View()
	command := ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start"}
	first, err := s.Reconcile(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	view := manager.View()
	latest := latestReconcileObservation(view, callID)
	command.ObservationID, command.ExpectedRevision = latest.ID, view.LastSeq
	second, err := s.Reconcile(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if first.OperationID == second.OperationID || calls.Load() != 2 || len(manager.View().Reconciliations) != 2 {
		t.Fatalf("repeated unknown reconciliation did not append distinct facts: first=%+v second=%+v calls=%d view=%+v", first, second, calls.Load(), manager.View())
	}
}

func TestReconcileStaleQueryFailsOperationWithoutOverwritingLateEvidence(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s, manager, _, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
		close(started)
		<-release
		return ReconcileEvidence{EvidenceRefs: []string{"stale-query"}, EvidenceSource: "test-query", TrustedNoStart: true}, nil
	}))
	before := manager.View()
	result := make(chan struct {
		receipt state.OperationReceipt
		err     error
	}, 1)
	go func() {
		receipt, err := s.Reconcile(context.Background(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start"})
		result <- struct {
			receipt state.OperationReceipt
			err     error
		}{receipt, err}
	}()
	<-started
	if err := manager.AppendObservation(context.Background(), 1, state.ObservationRevision{ID: "late", CallID: callID, Version: 2, PreviousID: before.Observations["legacy:"+callID].ID, Observation: agent.ToolObservation{Status: "succeeded", SideEffect: "confirmed", Executed: true}}, nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	out := <-result
	status, err := s.GetOperation(context.Background(), out.receipt.OperationID)
	latest := latestReconcileObservation(manager.View(), callID)
	if out.err == nil || err != nil || status.State != "failed" || status.ErrorRef != product.CodeStateConflict || latest.ID != "late" || manager.View().Calls[callID].Observation.SideEffect != "unknown" {
		t.Fatalf("stale reconciliation overwrote late evidence or remained active: receipt=%+v resultErr=%v status=%+v statusErr=%v latest=%+v view=%+v", out.receipt, out.err, status, err, latest, manager.View())
	}
}

func TestReconcileCommitsEvidenceAfterCallerCancellation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s, manager, _, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
		close(started)
		<-release
		return ReconcileEvidence{EvidenceRefs: []string{"query-evidence"}, EvidenceSource: "test-query"}, nil
	}))
	before := manager.View()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan struct {
		receipt state.OperationReceipt
		err     error
	}, 1)
	go func() {
		receipt, err := s.Reconcile(ctx, ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start"})
		result <- struct {
			receipt state.OperationReceipt
			err     error
		}{receipt, err}
	}()
	<-started
	cancel()
	close(release)
	out := <-result
	if out.receipt.OperationID == "" {
		t.Fatalf("cancelled reconcile lost accepted receipt: %+v err=%v", out.receipt, out.err)
	}
	status, err := s.GetOperation(context.Background(), out.receipt.OperationID)
	if err != nil || status.State != "completed" || len(manager.View().Reconciliations) != 1 {
		t.Fatalf("cancelled reconcile left durable operation incomplete: status=%+v err=%v resultErr=%v view=%+v", status, err, out.err, manager.View())
	}
}
