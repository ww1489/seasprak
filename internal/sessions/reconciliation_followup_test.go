package sessions

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func TestReconcileEffectiveObservationIsUsedByLookupAndFinish(t *testing.T) {
	var queries atomic.Int32
	s, manager, _, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
		queries.Add(1)
		return ReconcileEvidence{EvidenceRefs: []string{"confirmed-evidence"}, EvidenceSource: "query:confirmed", ConfirmedExecution: true}, nil
	}))
	before := manager.View()
	_, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start"})
	if err != nil {
		t.Fatal(err)
	}
	call := manager.View().Calls[callID]
	_, err = s.rt.call(t.Context(), func(rt *runtime) (any, error) {
		rt.active = &execution{scope: agent.ExecutionScope{SessionID: call.Scope.SessionID, BranchID: call.Scope.BranchID, TraceID: call.Scope.TraceID, InvocationID: call.Scope.InvocationID, ExecutionID: "lookup-only", Generation: call.Scope.Generation}, turnID: "turn"}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = s.rt.call(context.Background(), func(rt *runtime) (any, error) { rt.active = nil; return nil, nil })
	}()
	lookup, err := s.rt.LookupTool(t.Context(), s.rt.active.scope, call.Call.ProviderCallID)
	if err != nil || lookup.Observation == nil || lookup.Observation.Content != "reconciled execution confirmed" {
		t.Fatalf("lookup projected stale observation: %+v err=%v", lookup, err)
	}
	if err := manager.FinishTools(t.Context(), manager.View().Turns["turn"]); err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, message := range manager.View().Messages {
		if message.Kind != agent.KindToolResult || message.Standard == nil {
			continue
		}
		for _, block := range message.Standard.ContentBlocks {
			if block.FunctionToolResult != nil && len(block.FunctionToolResult.Content) == 1 && block.FunctionToolResult.Content[0].Text != nil {
				results = append(results, block.FunctionToolResult.Content[0].Text.Text)
			}
		}
	}
	if len(results) != 1 || results[0] != "reconciled execution confirmed" || queries.Load() != 1 || manager.View().Calls[callID].Observation.Content == "reconciled execution confirmed" {
		t.Fatalf("tool result projection or original fact changed: results=%v queries=%d", results, queries.Load())
	}
}

func TestReconcileRemainingUnknownRetainsHoldAndBlocksNewWork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence ReconcileEvidence
	}{
		{"confirmed-with-unknown", ReconcileEvidence{ConfirmedExecution: true}},
		{"no-start-with-unknown", ReconcileEvidence{TrustedNoStart: true}},
		{"no-start-and-execution", ReconcileEvidence{TrustedNoStart: true, ConfirmedExecution: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queries atomic.Int32
			s, manager, scheduler, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
				queries.Add(1)
				evidence := tc.evidence
				evidence.EvidenceRefs = []string{"evidence"}
				evidence.EvidenceSource = "query:conflict"
				if tc.name != "no-start-and-execution" {
					evidence.RemainingUnknown = []string{"effect still unknown"}
				}
				return evidence, nil
			}))
			before := manager.View()
			receipt, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start"})
			if err != nil {
				t.Fatal(err)
			}
			status, err := s.GetOperation(t.Context(), receipt.OperationID)
			if err != nil || status.State != "completed" || status.Result == nil {
				t.Fatalf("reconciliation state: %+v err=%v", status, err)
			}
			view := manager.View()
			if !view.HasUnresolvedEffects() || !scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)) || queries.Load() != 1 || view.Traces[input.TraceID].Usage.ToolExecutions != before.Traces[input.TraceID].Usage.ToolExecutions {
				t.Fatalf("conflicting evidence cleared unknown or released hold: status=%+v queries=%d", status, queries.Load())
			}
			_, err = s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"next"}`)})
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeReconciliationRequired {
				t.Fatalf("new work was accepted with unknown effects: %v", err)
			}
		})
	}
}

func TestLateConfirmedEvidenceAfterTrustedNoStartBlocksWorkAndRestoresHold(t *testing.T) {
	s, manager, scheduler, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
		return ReconcileEvidence{EvidenceRefs: []string{"no-start-evidence"}, EvidenceSource: "query:no-start", TrustedNoStart: true}, nil
	}))
	before := manager.View()
	if _, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start"}); err != nil {
		t.Fatal(err)
	}
	if scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)) {
		t.Fatal("trusted no-start did not release its hold")
	}
	late := manager.View().Calls[callID]
	late.Observation = &agent.ToolObservation{Status: "succeeded", SideEffect: "confirmed", Executed: true, Content: "late confirmed effect"}
	payload, err := json.Marshal(late)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.rt.CommitFact(t.Context(), late.Scope, agent.Fact{Kind: "tool_observation", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if !scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)) {
		t.Fatal("late confirmed effect did not immediately restore its resource hold")
	}
	view := manager.View()
	if !view.HasUnresolvedEffects() || !view.TraceHasUnresolvedEffects(input.TraceID) || view.Calls[callID].Observation.SideEffect != "unknown" || latestReconcileObservation(view, callID).Observation.SideEffect != "confirmed" {
		t.Fatalf("late confirmed fact was not kept as conflicting evidence: %+v", view)
	}
	_, err = s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"next"}`)})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeReconciliationRequired {
		t.Fatalf("conflicting evidence allowed new work: %v", err)
	}
	restored := tools.NewResourceScheduler()
	reopen := &runtime{opts: Options{SessionID: "reconcile-session", Workspace: s.rt.opts.Workspace, ResourceScheduler: restored}, manager: manager}
	if err := reopen.restoreResourceHolds(); err != nil || !restored.HasHold(tools.ResourceHoldID("reconcile-session", callID)) {
		t.Fatalf("conflicting late effect did not restore hold: %v", err)
	}
}

func TestReconcileLateConflictCanBeResolvedByNewConfirmedEvidence(t *testing.T) {
	var queries atomic.Int32
	s, manager, scheduler, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
		if queries.Add(1) == 1 {
			return ReconcileEvidence{EvidenceRefs: []string{"no-start"}, EvidenceSource: "query:first", TrustedNoStart: true}, nil
		}
		return ReconcileEvidence{EvidenceRefs: []string{"late-confirmed"}, EvidenceSource: "query:second", ConfirmedExecution: true}, nil
	}))
	before := manager.View()
	if _, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start"}); err != nil {
		t.Fatal(err)
	}
	previous := latestReconcileObservation(manager.View(), callID)
	if err := manager.AppendObservation(t.Context(), previous.Version, state.ObservationRevision{ID: "late-actual", CallID: callID, Version: previous.Version + 1, PreviousID: previous.ID, Observation: agent.ToolObservation{Status: "succeeded", SideEffect: "confirmed", Executed: true, Content: "late effect"}}, nil); err != nil {
		t.Fatal(err)
	}
	conflicted := manager.View()
	if !conflicted.HasUnresolvedEffects() {
		t.Fatal("late effect did not require renewed reconciliation")
	}
	if _, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: conflicted.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: "late-actual", ObservationVersion: 3, ExpectedRevision: conflicted.LastSeq, QueryID: "no-start"}); err != nil {
		t.Fatal(err)
	}
	view := manager.View()
	if queries.Load() != 2 || view.HasUnresolvedEffects() || !scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)) || latestReconcileObservation(view, callID).Observation.SideEffect != "confirmed" {
		t.Fatalf("renewed reconciliation did not retain confirmed effect and hold: queries=%d unresolved=%t hold=%t effect=%s result=%+v", queries.Load(), view.HasUnresolvedEffects(), scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)), latestReconcileObservation(view, callID).Observation.SideEffect, view.Reconciliations)
	}
}

func TestReconcileRejectsStoppedProofChangedDuringQuery(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s, manager, scheduler, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
		close(started)
		<-release
		return ReconcileEvidence{EvidenceRefs: []string{"evidence"}, EvidenceSource: "query:nostart", TrustedNoStart: true}, nil
	}))
	before := manager.View()
	result := make(chan error, 1)
	go func() {
		_, err := s.Reconcile(context.Background(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: before.Observations["legacy:"+callID].ID, ExpectedRevision: before.LastSeq, QueryID: "no-start"})
		result <- err
	}()
	<-started
	// A coordinator may advance the trace while a trusted query runs outside the mailbox.
	// Moving it to cancelling invalidates the accepted paused/stopped snapshot.
	if err := manager.SetTraceState(context.Background(), input.TraceID, "cancelling", false); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("stale trace state allowed terminal reconciliation")
	}
	if len(manager.View().Reconciliations) != 0 || !scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)) {
		t.Fatal("stale reconciliation released a hold or wrote a terminal observation")
	}
}
