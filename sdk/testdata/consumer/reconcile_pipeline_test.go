package consumer_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/sdk"
)

// Use only public session operations to create, identify and reconcile an
// unknown result. No internal manager or fabricated observation is involved.
func TestSDKConsumerReconcileUnknownThroughSnapshot(t *testing.T) {
	var runs, queries atomic.Int32
	model := &consumerOutputModel{}
	def := sdk.ToolDefinition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		return "", errors.New("controlled result unavailable")
	}}
	def.Execution.Effect, def.Execution.BackendID = "read", "trusted-run"
	var request sdk.ReconcileQueryRequest
	opts := sdk.SessionOptions{SessionID: "consumer-reconcile-pipeline", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sdk.ProfileMemory, Principal: "host", Model: model, Tools: []sdk.ToolDefinition{def}, ReconcileQueries: map[string]sdk.ReconcileQuery{
		"no-start": sdk.ReconcileQueryFunc(func(_ context.Context, req sdk.ReconcileQueryRequest) (sdk.ReconcileEvidence, error) {
			queries.Add(1)
			request = req
			return sdk.ReconcileEvidence{EvidenceRefs: []string{"controlled-no-start"}, EvidenceSource: "consumer-query", TrustedNoStart: true}, nil
		}),
	}}
	s, err := sdk.CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	input, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"work"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var before sdk.Snapshot
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		before, err = s.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		tr := before.Traces[input.TraceID]
		if tr.ExecutionStopped && (tr.State == "failed" || tr.State == "completed" || tr.State == "cancelled") {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("unknown execution did not durably stop")
		default:
			runtime.Gosched()
		}
	}
	if runs.Load() != 1 || len(before.Calls) != 1 || len(before.Observations) != 1 || before.Traces[input.TraceID].Usage.ToolExecutions != 1 {
		t.Fatal("fixture did not produce one durable tool execution")
	}
	var original sdk.ObservationView
	for _, observation := range before.Observations {
		original = observation
	}
	if original.Version != 1 || original.Observation.SideEffect != "unknown" || before.Calls[original.CallID].Observation == nil {
		t.Fatal("public snapshot lost the unknown observation identity")
	}
	cmd := sdk.ReconcileCommand{TraceID: input.TraceID, InvocationID: before.Traces[input.TraceID].InvocationID, CallID: original.CallID, ObservationID: original.ID, ObservationVersion: original.Version, ExpectedRevision: before.Revision, QueryID: "no-start", IdempotencyKey: "consumer-reconcile-once"}
	stale := cmd
	stale.ExpectedRevision--
	if _, err := s.Reconcile(t.Context(), stale); err == nil {
		t.Fatal("stale revision was accepted")
	} else if pe, ok := sdk.AsError(err); !ok || pe.Code != sdk.CodeStateConflict {
		t.Fatalf("stale revision code: %v", err)
	}
	unchanged, err := s.Snapshot(t.Context())
	if err != nil || unchanged.Revision != before.Revision || queries.Load() != 0 {
		t.Fatal("rejected revision changed state or queried evidence")
	}
	receipt, err := s.Reconcile(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := s.GetOperation(t.Context(), receipt.OperationID)
	if err != nil || operation.State != "completed" || operation.Result == nil {
		t.Fatalf("reconcile operation state=%s err=%v", operation.State, err)
	}
	after, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if request.CallID != original.CallID || request.ObservationID != original.ID || request.ObservationVersion != 1 || request.InvocationID != cmd.InvocationID || queries.Load() != 1 {
		t.Fatal("query was not bound to the public original identity")
	}
	if len(after.Observations) != 2 || !reflect.DeepEqual(after.Observations[original.ID], original) || !reflect.DeepEqual(after.Calls, before.Calls) {
		t.Fatal("reconciliation overwrote original facts")
	}
	var latest sdk.ObservationView
	for _, observation := range after.Observations {
		if observation.Version > latest.Version {
			latest = observation
		}
	}
	if latest.Version != 2 || latest.PreviousID != original.ID || latest.CallID != original.CallID || latest.Observation.SideEffect != "none" || latest.Observation.Executed {
		t.Fatal("trusted no-start did not append a linked observation")
	}
	modelCalls := model.calls.Load()
	if runs.Load() != 1 || after.Traces[input.TraceID].State != before.Traces[input.TraceID].State || after.Traces[input.TraceID].Usage.ToolExecutions != 1 {
		t.Fatal("reconciliation resumed or reexecuted work")
	}
	retry, err := s.Reconcile(t.Context(), cmd)
	if err != nil || retry.OperationID != receipt.OperationID || queries.Load() != 1 {
		t.Fatal("idempotent retry repeated the query")
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opened, err := sdk.OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	reopened, err := opened.Snapshot(t.Context())
	if err != nil || !reflect.DeepEqual(reopened.Observations, after.Observations) || runs.Load() != 1 || model.calls.Load() != modelCalls || queries.Load() != 1 {
		t.Fatal("reopen lost observation versions or executed work")
	}
	persisted, err := opened.GetOperation(t.Context(), receipt.OperationID)
	if err != nil || persisted.State != "completed" {
		t.Fatal("reopen lost the completed reconciliation operation")
	}
}
