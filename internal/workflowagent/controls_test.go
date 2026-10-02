package workflowagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestWorkflowPauseWaitsAndReusesCompletedNode(t *testing.T) {
	var calls atomic.Int32
	opts := testOptions(t, modelThenTool(), testkit.NewFake(testkit.Step{Text: "ok"}), &calls)
	// Reverse the two effect nodes: the running tool finishes before pause.
	d := opts.Definition
	d.Nodes[1], d.Nodes[2] = d.Nodes[2], d.Nodes[1]
	d.Nodes[1].Inputs = map[string]WorkflowValue{"q": {Literal: json.RawMessage(`"q"`)}}
	d.Edges = []WorkflowEdge{{From: "s", To: "t"}, {From: "t", To: "m"}, {From: "m", To: "e"}}
	d.Nodes[3].Inputs = map[string]WorkflowValue{"result": output("m", "text")}
	opts.Definition = d
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	opts.Tools[0].Run = func(context.Context, json.RawMessage) (string, error) {
		calls.Add(1)
		close(entered)
		<-release
		return "tool-result", nil
	}
	w := newWorkflow(t, opts)
	submit(t, w)
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := w.Pause(ctx, WorkflowControlCommand{IdempotencyKey: "pause", Principal: "local"})
	if err != context.DeadlineExceeded {
		t.Fatalf("pause wait %v", err)
	}
	s, _ := w.Snapshot(t.Context())
	if s.ExecutionStopped || s.State != "pausing" {
		t.Fatalf("premature stop %+v", s)
	}
	release <- struct{}{}
	s = waitStopped(t, w)
	if s.State != "paused" || calls.Load() != 1 || s.Usage.LogicalModelCalls != 0 {
		t.Fatalf("paused %+v", s)
	}
	_, err = w.Resume(t.Context(), WorkflowControlCommand{IdempotencyKey: "resume", Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	s = waitStopped(t, w)
	if s.State != "completed" || calls.Load() != 1 || s.Usage.LogicalModelCalls != 1 {
		t.Fatalf("resumed %+v", s)
	}
}
func TestWorkflowCancelWaitTimeoutDoesNotProveExit(t *testing.T) {
	var calls atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &calls)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	opts.Tools[0].Run = func(ctx context.Context, _ json.RawMessage) (string, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		<-release
		return "", ctx.Err()
	}
	w := newWorkflow(t, opts)
	submit(t, w)
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := w.Cancel(ctx, WorkflowControlCommand{IdempotencyKey: "cancel", Principal: "local"})
	if err != context.DeadlineExceeded {
		t.Fatalf("cancel wait %v", err)
	}
	s, _ := w.Snapshot(t.Context())
	if s.ExecutionStopped || s.State != "cancelling" {
		t.Fatalf("premature cancel %+v", s)
	}
	release <- struct{}{}
	s = waitStopped(t, w)
	if s.State != "cancelled" || calls.Load() != 1 {
		t.Fatalf("cancel %+v", s)
	}
	before := s.Revision
	_, err = w.Cancel(t.Context(), WorkflowControlCommand{IdempotencyKey: "cancel", Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ = w.Snapshot(t.Context())
	if s.Revision != before {
		t.Fatal("cancel finalized twice")
	}
	_, err = w.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
	requireCode(t, err, product.CodeIncompatibleResume)
}
func TestWorkflowInstanceApprovalExplicitResumeAndReopen(t *testing.T) {
	var calls atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &calls)
	opts.Tools[0].Execution.RequestedGrantRef = "once"
	w := newWorkflow(t, opts)
	submit(t, w)
	s := waitStopped(t, w)
	if s.State != "paused" || calls.Load() != 0 || len(s.Interactions) != 1 {
		t.Fatalf("approval wait %+v", s)
	}
	var q WorkflowInteraction
	for _, q = range s.Interactions {
	}
	r, err := w.RespondInteraction(t.Context(), WorkflowInteractionResponse{InteractionID: q.ID, Decision: "allowed-once", ExpectedRevision: s.Revision, IdempotencyKey: "answer", Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if r.AcceptedCommit != 0 || r.ReceiptScope != "instance" || r.InstanceID != s.InstanceID {
		t.Fatalf("approval receipt %+v", r)
	}
	if calls.Load() != 0 {
		t.Fatal("answer auto-resumed")
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	w, err = OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	s, _ = w.Snapshot(t.Context())
	if len(s.Interactions) != 0 || s.InstanceID == q.InstanceID {
		t.Fatal("approval survived instance")
	}
	_, err = w.RespondInteraction(t.Context(), WorkflowInteractionResponse{InteractionID: q.ID, Decision: "allowed-once", ExpectedRevision: s.Revision, Principal: "local"})
	requireCode(t, err, product.CodeNotFound)
	_, err = w.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	s = waitStopped(t, w)
	if s.State != "paused" || calls.Load() != 0 || len(s.Interactions) != 1 {
		t.Fatalf("reopen did not reask %+v", s)
	}
	for _, q = range s.Interactions {
	}
	_, err = w.RespondInteraction(t.Context(), WorkflowInteractionResponse{InteractionID: q.ID, Decision: "allowed-once", ExpectedRevision: s.Revision, Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	s = waitStopped(t, w)
	if s.State != "completed" || calls.Load() != 1 || s.Usage.ToolExecutions != 1 {
		t.Fatalf("approved %+v", s)
	}
}
