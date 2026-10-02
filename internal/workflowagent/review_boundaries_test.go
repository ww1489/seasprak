package workflowagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestWorkflowApprovalResumeReusesFrozenPreparedArguments(t *testing.T) {
	var effects, preparations, resolutions atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &effects)
	opts.Tools[0].Execution.RequestedGrantRef = "one-operation"
	opts.Tools[0].PrepareArguments = []func(context.Context, json.RawMessage) (json.RawMessage, error){func(context.Context, json.RawMessage) (json.RawMessage, error) {
		if preparations.Add(1) == 1 {
			return json.RawMessage(`{"q":"A"}`), nil
		}
		return json.RawMessage(`{"q":"B"}`), nil
	}}
	opts.Tools[0].ResolveExecution = func(_ context.Context, _ json.RawMessage, description tools.ExecutionDescription) (tools.ExecutionDescription, error) {
		resolutions.Add(1)
		return description, nil
	}
	opts.Tools[0].Run = func(_ context.Context, raw json.RawMessage) (string, error) {
		effects.Add(1)
		if string(raw) != `{"q":"A"}` {
			t.Error("approved tool executed replacement arguments instead of the original frozen arguments")
		}
		return "original-A", nil
	}
	w := newWorkflow(t, opts)
	submit(t, w)
	waiting := waitStopped(t, w)
	if waiting.State != "paused" || len(waiting.Interactions) != 1 || preparations.Load() != 1 || resolutions.Load() != 1 || effects.Load() != 0 {
		t.Fatalf("wrong initial freeze: state=%s prepare=%d resolve=%d effects=%d", waiting.State, preparations.Load(), resolutions.Load(), effects.Load())
	}
	var question WorkflowInteraction
	for _, question = range waiting.Interactions {
	}
	_, err := w.RespondInteraction(t.Context(), WorkflowInteractionResponse{InteractionID: question.ID, Decision: "allowed-once", ExpectedRevision: waiting.Revision, IdempotencyKey: "allow-original-A", Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if effects.Load() != 0 {
		t.Fatal("answer implicitly executed the tool")
	}
	_, err = w.Resume(t.Context(), WorkflowControlCommand{Principal: "local", IdempotencyKey: "resume-original-A"})
	if err != nil {
		t.Fatal(err)
	}
	final := waitStopped(t, w)
	if final.State != "completed" || effects.Load() != 1 || preparations.Load() != 1 || resolutions.Load() != 1 {
		t.Errorf("recovery did not reuse original frozen call: state=%s code=%s prepare=%d resolve=%d effects=%d", final.State, final.ErrorCode, preparations.Load(), resolutions.Load(), effects.Load())
	}
}

func TestWorkflowResumeBeforeApprovalKeepsRespondableWait(t *testing.T) {
	var effects atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &effects)
	opts.Tools[0].Execution.RequestedGrantRef = "one-operation"
	w := newWorkflow(t, opts)
	submit(t, w)
	before := waitStopped(t, w)
	if before.State != "paused" || len(before.Interactions) != 1 || effects.Load() != 0 {
		t.Fatalf("expected stopped approval wait, snapshot=%+v effects=%d", before, effects.Load())
	}
	if before.CanResume {
		t.Error("snapshot advertised resume while the original approval was unanswered")
	}
	var question WorkflowInteraction
	for _, question = range before.Interactions {
	}
	_, err := w.Resume(t.Context(), WorkflowControlCommand{Principal: "local", IdempotencyKey: "unanswered-resume"})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict {
		t.Errorf("unanswered Resume error=%v, want state_conflict", err)
	}
	if err == nil {
		// Confirm the actual incorrectly admitted segment exits before inspecting
		// the original waiting call; a command receipt is not exit evidence.
		waitStopped(t, w)
	}
	after, err := w.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.State != "paused" || !after.ExecutionStopped || effects.Load() != 0 {
		t.Errorf("unanswered recovery changed waiting facts: revision=%d->%d state=%s stopped=%t effects=%d", before.Revision, after.Revision, after.State, after.ExecutionStopped, effects.Load())
	}
	for _, node := range after.WorkflowNodes {
		if node.Kind == "tool" && node.State != "waiting" {
			t.Errorf("original approval node became %s, want waiting", node.State)
		}
	}
	_, err = w.RespondInteraction(t.Context(), WorkflowInteractionResponse{InteractionID: question.ID, Decision: "allowed-once", ExpectedRevision: after.Revision, IdempotencyKey: "answer-original", Principal: "local"})
	if err != nil {
		t.Fatalf("original waiting interaction cannot be answered: %v", err)
	}
	if effects.Load() != 0 {
		t.Fatal("answer implicitly executed the tool")
	}
	_, err = w.Resume(t.Context(), WorkflowControlCommand{Principal: "local", IdempotencyKey: "answered-resume"})
	if err != nil {
		t.Fatal(err)
	}
	final := waitStopped(t, w)
	if final.State != "completed" || effects.Load() != 1 || !json.Valid(final.Result) {
		t.Fatalf("original approved call did not complete exactly once: state=%s effects=%d", final.State, effects.Load())
	}
}
