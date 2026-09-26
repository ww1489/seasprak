package consumer_test

import (
	"context"
	"encoding/json"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/sdk"
)

type sdkApprovalResponder interface {
	RespondInteraction(context.Context, sdk.InteractionResponse) (sdk.OperationReceipt, error)
}

func TestSDKConsumerApprovesOriginalCallAfterReopenAndExplicitlyResumes(t *testing.T) {
	var runs atomic.Int32
	initialModel := &consumerResumeModel{}
	def := sdk.ToolDefinition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		return "approved result", nil
	}}
	def.Execution.Effect, def.Execution.RequestedGrantRef = "read", "one-operation"
	opts := sdk.SessionOptions{SessionID: "consumer-approval", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sdk.ProfileMemory, Principal: "host-user", Model: initialModel, Tools: []sdk.ToolDefinition{def}, GenerationFingerprint: "consumer-approval-bundle-v1"}
	s, err := sdk.CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	input, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	var before sdk.Snapshot
	for {
		before, err = s.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if tr := before.Traces[input.TraceID]; tr.State == "paused" || tr.State == "failed" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("approval did not safely wait")
		default:
			runtime.Gosched()
		}
	}
	if before.Traces[input.TraceID].State != "paused" || len(before.Interactions) != 1 || runs.Load() != 0 || initialModel.calls.Load() != 1 {
		t.Fatalf("SDK cannot query pending approval: trace=%+v interactions=%d runs=%d", before.Traces[input.TraceID], len(before.Interactions), runs.Load())
	}
	var interaction sdk.Interaction
	for _, in := range before.Interactions {
		interaction = in
		frozen := before.FrozenExecutions[before.Approvals[in.ApprovalID].FrozenExecutionID]
		if in.State != "ready" || in.TargetRef != "" || in.CheckpointRef == "" || frozen.Hash == "" || frozen.Hash != before.Approvals[in.ApprovalID].FrozenHash {
			t.Fatalf("SDK exposes an incomplete or private approval mapping: %+v", in)
		}
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	resumedModel := &consumerResumeModel{final: true}
	opts.Model = resumedModel
	opened, err := sdk.OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	responder, supported := any(opened).(sdkApprovalResponder)
	if !supported {
		t.Fatal("SDK session cannot durably respond to an approval")
	}
	snap, err := opened.Snapshot(t.Context())
	if err != nil || snap.Interactions[interaction.ID].ID != interaction.ID || resumedModel.calls.Load() != 0 || runs.Load() != 0 {
		t.Fatalf("open changed original interaction or executed work: %v", err)
	}
	cmd := sdk.InteractionResponse{InteractionID: interaction.ID, Decision: "allowed-once", ExpectedRevision: snap.Revision, IdempotencyKey: "answer"}
	receipt, err := responder.RespondInteraction(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	status, err := opened.GetOperation(t.Context(), receipt.OperationID)
	if err != nil || status.State != "completed" || runs.Load() != 0 || resumedModel.calls.Load() != 0 {
		t.Fatalf("answer executed work or lost status: %+v %v", status, err)
	}
	snap, err = opened.Snapshot(t.Context())
	if err != nil || snap.Interactions[interaction.ID].State != "allowed-once" || !snap.Resume[input.TraceID].CanResume {
		t.Fatalf("answered snapshot=%+v eligibility=%+v err=%v", snap.Interactions, snap.Resume, err)
	}
	if _, err := opened.Resume(t.Context(), sdk.ResumeCommand{TraceID: input.TraceID, ExpectedRevision: snap.Revision}); err != nil {
		t.Fatal(err)
	}
	deadline = time.After(5 * time.Second)
	for {
		snap, err = opened.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if tr := snap.Traces[input.TraceID]; tr.State == "completed" || tr.State == "failed" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("approved resume did not complete")
		default:
			runtime.Gosched()
		}
	}
	again, err := responder.RespondInteraction(t.Context(), cmd)
	if err != nil || again != receipt || snap.Traces[input.TraceID].State != "completed" || runs.Load() != 1 || resumedModel.calls.Load() != 1 || initialModel.calls.Load() != 1 || snap.Traces[input.TraceID].Usage.ToolExecutions != 1 || snap.Approvals[interaction.ApprovalID].State != "claimed" {
		t.Fatalf("answer or resume changed original execution: receipt=%+v again=%+v trace=%+v runs=%d models=%d/%d err=%v", receipt, again, snap.Traces[input.TraceID], runs.Load(), initialModel.calls.Load(), resumedModel.calls.Load(), err)
	}
}
