package sessions_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestPausePersistsCheckpointAndOpenDoesNotResume(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var runs atomic.Int32
	model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "must not auto-run"})
	opts := sessions.Options{SessionID: "pause-reopen", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sessions.ProfileMemory, Model: model, Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		close(entered)
		<-release
		return "ok", nil
	}}}}
	s, err := sessions.CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	receipt := submit(t, s)
	select {
	case <-entered:
	case <-time.After(4 * time.Second):
		t.Fatal("tool not entered")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	accepted, err := s.Pause(waitCtx, receipt.TraceID)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || accepted.OperationID == "" {
		t.Fatalf("pause wait=%v receipt=%+v", err, accepted)
	}
	snap, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Traces[receipt.TraceID].State != "running" || snap.Traces[receipt.TraceID].Settled {
		t.Fatalf("pause settled while tool still runs: %+v", snap.Traces[receipt.TraceID])
	}
	pending, err := s.GetOperation(t.Context(), accepted.OperationID)
	if err != nil || pending.State != "accepted" || pending.ResultRef != "" || pending.OperationReceipt != accepted {
		t.Fatalf("accepted operation query=%+v err=%v", pending, err)
	}
	close(release)
	deadline := time.After(5 * time.Second)
	for {
		snap, err = s.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if snap.Traces[receipt.TraceID].State == "paused" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("pause did not persist: %+v", snap.Traces[receipt.TraceID])
		case <-time.After(time.Millisecond):
		}
	}
	if snap.Traces[receipt.TraceID].Settled || !snap.Traces[receipt.TraceID].ExecutionStopped || runs.Load() != 1 || model.Calls() != 1 {
		t.Fatalf("pause state=%+v runs=%d model=%d", snap.Traces[receipt.TraceID], runs.Load(), model.Calls())
	}
	completed, err := s.GetOperation(t.Context(), accepted.OperationID)
	if err != nil || completed.State != "completed" || completed.ResultRef == "" || completed.Revision != pending.Revision+1 {
		t.Fatalf("completed operation query=%+v err=%v", completed, err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopenedModel := testkit.NewFake(testkit.Step{Text: "must not auto-run"})
	opts.Model = reopenedModel
	opened, err := sessions.OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close(context.Background())
	status, err := opened.GetOperation(t.Context(), accepted.OperationID)
	if err != nil || status != completed || status.OperationID != accepted.OperationID || status.Target != accepted.Target || status.AcceptedCommit != accepted.AcceptedCommit {
		t.Fatalf("reopened operation changed: got=%+v want=%+v err=%v", status, completed, err)
	}
	after, err := opened.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.Traces[receipt.TraceID].State != "paused" || after.Traces[receipt.TraceID].Settled || reopenedModel.Calls() != 0 || runs.Load() != 1 {
		t.Fatalf("open restarted paused trace: %+v model=%d tools=%d", after.Traces[receipt.TraceID], reopenedModel.Calls(), runs.Load())
	}
}
