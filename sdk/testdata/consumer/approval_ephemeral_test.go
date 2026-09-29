package consumer_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/sdk"
)

func TestSDKConsumerOneTimeApprovalDoesNotSurviveReopen(t *testing.T) {
	var runs atomic.Int32
	initialModel := &consumerResumeModel{}
	def := sdk.ToolDefinition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		return "approved result", nil
	}}
	def.Execution.Effect, def.Execution.RequestedGrantRef = "read", "one-operation"
	opts := sdk.SessionOptions{SessionID: "consumer-ephemeral-approval", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sdk.ProfileMemory, Principal: "host-user", Model: initialModel, Tools: []sdk.ToolDefinition{def}, GenerationFingerprint: "consumer-ephemeral-approval-v1"}
	s, err := sdk.CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	input, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	before := waitConsumerApprovalState(t, s, input.TraceID, "paused")
	in := onlyConsumerApproval(t, before)
	answer := sdk.InteractionResponse{InteractionID: in.ID, Decision: "allowed-once", ExpectedRevision: before.Revision, IdempotencyKey: "answer"}
	receipt, err := s.RespondInteraction(t.Context(), answer)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.RespondInteraction(t.Context(), answer); err != nil || again != receipt {
		t.Fatalf("same-runtime answer must be idempotent: equal=%t err=%v", again == receipt, err)
	}
	answered, err := s.Snapshot(t.Context())
	if err != nil || answered.Revision != before.Revision || receipt.AcceptedCommit != 0 {
		t.Fatalf("approval answer must not create a durable commit: before=%d after=%d acceptedCommit=%d err=%v", before.Revision, answered.Revision, receipt.AcceptedCommit, err)
	}
	if runs.Load() != 0 || initialModel.calls.Load() != 1 {
		t.Fatal("answer started execution before explicit resume")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	reopenedModel := &consumerResumeModel{final: true}
	opts.Model = reopenedModel
	opened, err := sdk.OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	snap, err := opened.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Interactions) != 0 || len(snap.Approvals) != 0 || snap.Revision != before.Revision {
		t.Fatal("Open created approval requests or changed the journal before Resume")
	}
	if _, staleErr := opened.RespondInteraction(t.Context(), answer); staleErr == nil {
		t.Fatal("late retry of the previous runtime's answer was accepted")
	} else if pe, ok := sdk.AsError(staleErr); !ok || (pe.Code != sdk.CodeNotFound && pe.Code != sdk.CodeStateConflict) {
		t.Fatalf("late answer returned an unexpected error: %v", staleErr)
	}
	_, statusErr := opened.GetOperation(t.Context(), receipt.OperationID)
	if pe, ok := sdk.AsError(statusErr); !ok || pe.Code != sdk.CodeNotFound {
		t.Fatalf("transient approval receipt survived reopen: err=%v", statusErr)
	}
	if runs.Load() != 0 || reopenedModel.calls.Load() != 0 || snap.Traces[input.TraceID].Usage.ToolExecutions != 0 {
		t.Fatal("reopen executed work or consumed tool budget")
	}
	if err := filepath.WalkDir(opts.StateRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, forbidden := range []string{`"interaction"`, `"approval"`, `"approval_binding"`, `"approval_decision"`, `"approval_claim"`, `"respond_interaction"`, `"interaction.responded"`} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("new journal contains transient approval record type %s", forbidden)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := opened.Resume(t.Context(), sdk.ResumeCommand{TraceID: input.TraceID, ExpectedRevision: snap.Revision}); err != nil {
		t.Fatal(err)
	}
	snap = waitConsumerApprovalState(t, opened, input.TraceID, "paused")
	fresh := onlyConsumerApproval(t, snap)
	if fresh.State != "ready" || fresh.ID == in.ID || fresh.ApprovalID == in.ApprovalID || fresh.CallID != in.CallID || fresh.Scope != in.Scope {
		t.Fatal("explicit Resume reused permission identity or lost the original call")
	}
	if runs.Load() != 0 || reopenedModel.calls.Load() != 0 || snap.Traces[input.TraceID].Usage.ToolExecutions != 0 {
		t.Fatal("unanswered Resume executed work or consumed tool budget")
	}
	freshReceipt, err := opened.RespondInteraction(t.Context(), sdk.InteractionResponse{InteractionID: fresh.ID, Decision: "allowed-once", ExpectedRevision: snap.Revision, IdempotencyKey: "answer"})
	if err != nil || freshReceipt.OperationID == receipt.OperationID {
		t.Fatalf("reopen must accept a fresh decision, not replay the old one: same=%t err=%v", freshReceipt.OperationID == receipt.OperationID, err)
	}
	snap, err = opened.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.Resume(t.Context(), sdk.ResumeCommand{TraceID: input.TraceID, ExpectedRevision: snap.Revision}); err != nil {
		t.Fatal(err)
	}
	after := waitConsumerApprovalState(t, opened, input.TraceID, "completed")
	if runs.Load() != 1 || initialModel.calls.Load() != 1 || reopenedModel.calls.Load() != 1 || after.Traces[input.TraceID].Usage.ToolExecutions != 1 {
		t.Fatalf("fresh approval did not execute exactly once: tools=%d models=%d/%d usage=%d", runs.Load(), initialModel.calls.Load(), reopenedModel.calls.Load(), after.Traces[input.TraceID].Usage.ToolExecutions)
	}
}

func onlyConsumerApproval(t *testing.T, snap sdk.Snapshot) sdk.Interaction {
	t.Helper()
	if len(snap.Interactions) != 1 {
		t.Fatalf("expected one original pending interaction, got %d", len(snap.Interactions))
	}
	for _, in := range snap.Interactions {
		return in
	}
	panic("unreachable")
}

func waitConsumerApprovalState(t *testing.T, s *sdk.AgentSession, traceID, want string) sdk.Snapshot {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		snap, err := s.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		state := snap.Traces[traceID].State
		if state == want {
			return snap
		}
		if state == "failed" || state == "cancelled" {
			t.Fatalf("execution ended unexpectedly: state=%s", state)
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for state %s, got %s", want, state)
		default:
			runtime.Gosched()
		}
	}
}
