package codeagent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

type reconciliationReleaseStore struct {
	store.Store
	fail     bool
	attempts int
	commit   store.Commit
}

func (s *reconciliationReleaseStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	for _, record := range commit.ControlRecords {
		if record.Type == "reconciliation" {
			s.attempts++
			s.commit = commit
			if s.fail {
				return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "injected reconciliation append failure")
			}
		}
	}
	return s.Store.Append(ctx, id, expected, commit)
}

func TestReconcileDurableReleaseSharesCommitAndReplays(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "append-failed"}[fail], func(t *testing.T) {
			var queries atomic.Int32
			s, initial, scheduler, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
				queries.Add(1)
				return ReconcileEvidence{EvidenceRefs: []string{"no-start-proof"}, EvidenceSource: "trusted-executor", TrustedNoStart: true}, nil
			}))
			backend := &reconciliationReleaseStore{Store: s.rt.opts.Store, fail: fail}
			manager, err := state.NewManager(backend, "reconcile-session")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.manager = manager; return nil }); err != nil {
				t.Fatal(err)
			}
			before := initial.View()
			receipt, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: "legacy:" + callID, ExpectedRevision: before.LastSeq, QueryID: "no-start"})
			if fail {
				if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStorageUnavailable {
					t.Fatalf("append error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if queries.Load() != 1 || backend.attempts != 1 || s.rt.opts.Model.(*testkit.FakeModel).Calls() != 0 {
				t.Fatalf("query calls=%d append calls=%d model calls=%d", queries.Load(), backend.attempts, s.rt.opts.Model.(*testkit.FakeModel).Calls())
			}
			types := map[string]int{}
			for _, record := range backend.commit.ControlRecords {
				types[record.Type]++
			}
			if types["resource_hold_release"] != 1 || types["observation_revision"] != 1 || types["reconciliation"] != 1 || types["operation"] != 1 {
				t.Errorf("release must share the observation/reconciliation/operation commit: %v", types)
			}
			after := manager.View()
			if !reflect.DeepEqual(before.Calls, after.Calls) || !reflect.DeepEqual(before.Messages, after.Messages) || !reflect.DeepEqual(before.Traces, after.Traces) || before.Budget != after.Budget {
				t.Fatal("reconciliation rewrote history, claims, trace, or budget")
			}
			if fail && (!reflect.DeepEqual(before.Observations, after.Observations) || len(after.Reconciliations) != 0 || len(after.ResourceHoldReleases) != 0 || after.Operations[receipt.OperationID].State != "running") {
				t.Fatal("failed append published result or operation completion")
			}
			if scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)) != fail {
				t.Fatal("scheduler release did not follow durable commit")
			}
			reopened, err := state.NewManager(backend.Store, "reconcile-session")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, reopened.View()) {
				t.Fatal("journal replay differs from committed view")
			}
			restored := tools.NewResourceScheduler()
			rt := &runtime{opts: Options{SessionID: "reconcile-session", Workspace: s.rt.opts.Workspace, ResourceScheduler: restored}, manager: reopened}
			if err := rt.restoreResourceHolds(); err != nil {
				t.Fatal(err)
			}
			if restored.HasHold(tools.ResourceHoldID("reconcile-session", callID)) != fail {
				t.Fatal("reopen did not replay durable release state")
			}
		})
	}
}

func TestReconcileConflictingNoStartCannotWriteRelease(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence ReconcileEvidence
	}{
		{"unknown", ReconcileEvidence{TrustedNoStart: true, RemainingUnknown: []string{"unknown"}}},
		{"restriction", ReconcileEvidence{TrustedNoStart: true, ConflictRestrictions: []string{"conflict"}}},
		{"execution", ReconcileEvidence{TrustedNoStart: true, ConfirmedExecution: true}},
		{"effects", ReconcileEvidence{TrustedNoStart: true, ConfirmedEffects: []string{"file-written"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, manager, scheduler, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
				evidence := tc.evidence
				evidence.EvidenceRefs, evidence.EvidenceSource = []string{"proof"}, "executor"
				return evidence, nil
			}))
			before := manager.View()
			if _, err := s.Reconcile(t.Context(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: "legacy:" + callID, ExpectedRevision: before.LastSeq, QueryID: "no-start"}); err != nil {
				t.Fatal(err)
			}
			if !scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)) || !manager.View().HasUnresolvedEffects() {
				t.Fatal("conflicting no-start released its hold or cleared unknown effects")
			}
			stored, err := s.rt.opts.Store.Load(t.Context(), "reconcile-session")
			if err != nil {
				t.Fatal(err)
			}
			for _, commit := range stored.Commits {
				for _, record := range commit.ControlRecords {
					if record.Type == "resource_hold_release" {
						t.Fatal("conflicting evidence wrote a release record")
					}
				}
			}
		})
	}
}

func TestReconcileObservationAloneCannotReleaseRestoredHold(t *testing.T) {
	s, manager, _, _, callID := makeReconcileSession(t, nil)
	before := manager.View()
	receipt, err := manager.AcceptOperation(t.Context(), state.OperationCommand{Kind: "reconcile", Target: callID, ExpectedRevision: before.LastSeq, Content: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	first := before.Observations["legacy:"+callID]
	next := state.ObservationRevision{ID: "old-no-start", CallID: callID, Version: first.Version + 1, PreviousID: first.ID, Observation: agent.ToolObservation{Status: "cancelled", SideEffect: "none"}}
	result := state.Reconciliation{ID: "old-reconciliation", OperationID: receipt.OperationID, CallID: callID, ObservationID: first.ID, ObservationVersion: first.Version, NewObservationID: next.ID, QueryID: "no-start", EvidenceSource: "query", EvidenceRefs: []string{"old-proof"}}
	if err := manager.AppendObservation(t.Context(), first.Version, next, &result); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.NewManager(s.rt.opts.Store, "reconcile-session")
	if err != nil {
		t.Fatal(err)
	}
	scheduler := tools.NewResourceScheduler()
	rt := &runtime{opts: Options{SessionID: "reconcile-session", Workspace: s.rt.opts.Workspace, ResourceScheduler: scheduler}, manager: reopened}
	if err := rt.restoreResourceHolds(); err != nil {
		t.Fatal(err)
	}
	if !scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)) {
		t.Fatal("observation without a durable release freed the hold")
	}
}

func TestReconcileRejectsExecutionIDChangedDuringQuery(t *testing.T) {
	started, proceed := make(chan struct{}), make(chan struct{})
	s, manager, scheduler, input, callID := makeReconcileSession(t, ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
		close(started)
		<-proceed
		return ReconcileEvidence{EvidenceRefs: []string{"proof"}, EvidenceSource: "executor", TrustedNoStart: true}, nil
	}))
	before := manager.View()
	done := make(chan error, 1)
	go func() {
		_, err := s.Reconcile(context.Background(), ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: callID, ObservationID: "legacy:" + callID, ExpectedRevision: before.LastSeq, QueryID: "no-start"})
		done <- err
	}()
	<-started
	// A new execution may return to the same paused/stopped state during the query.
	if err := s.rt.do(t.Context(), func(rt *runtime) error {
		view := rt.manager.View()
		trace := *view.Traces[input.TraceID]
		trace.ExecutionID = "different-stopped-execution"
		raw, err := json.Marshal(trace)
		if err != nil {
			return err
		}
		_, err = rt.opts.Store.Append(t.Context(), rt.opts.SessionID, store.ExpectedCommit{ExpectedPreviousSeq: view.LastSeq}, store.Commit{RecordType: "commit", Version: 1, CommitID: "changed-execution", CommitSeq: view.LastSeq + 1, ExpectedPreviousSeq: view.LastSeq, ControlRecords: []store.Record{{Type: "trace", Version: 1, ID: trace.ID, Payload: raw}}})
		if err != nil {
			return err
		}
		rt.manager, err = state.NewManager(rt.opts.Store, rt.opts.SessionID)
		return err
	}); err != nil {
		close(proceed)
		<-done
		t.Fatal(err)
	}
	close(proceed)
	err := <-done
	var pe *product.Error
	if !errors.As(err, &pe) || pe.Code != product.CodeStateConflict {
		t.Fatalf("changed execution proof accepted: %v", err)
	}
	if len(s.rt.manager.View().Reconciliations) != 0 || !scheduler.HasHold(tools.ResourceHoldID("reconcile-session", callID)) {
		t.Fatal("changed execution released a hold")
	}
}
