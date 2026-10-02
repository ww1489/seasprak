package workflowagent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

type parallelStopModel struct {
	*testkit.FakeModel
	entered chan struct{}
	once    sync.Once
}

func (m *parallelStopModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	m.once.Do(func() { close(m.entered) })
	return m.FakeModel.Generate(ctx, in, opts...)
}

func TestWorkflowParallelApprovalCannotHideInvalidModelFailure(t *testing.T) {
	var effects atomic.Int32
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	m := &parallelStopModel{FakeModel: testkit.NewFake(testkit.Step{Text: "partial", NoFinish: true, Gate: gate}), entered: make(chan struct{})}
	d := modelThenTool()
	d.Nodes[2].Inputs = map[string]WorkflowValue{"q": {Literal: json.RawMessage(`"fixed"`)}}
	d.Nodes[3].Inputs = map[string]WorkflowValue{"result": {Literal: json.RawMessage(`"done"`)}}
	d.Edges = []WorkflowEdge{{From: "s", To: "m"}, {From: "s", To: "t"}, {From: "m", To: "e"}, {From: "t", To: "e"}}
	opts := testOptions(t, d, m, &effects)
	opts.Tools[0].Execution.RequestedGrantRef = "one-operation"
	// This test needs both siblings admitted: approval must not stop admission
	// before the real model request has entered its controlled response gate.
	opts.Tools[0].BeforeCall = []func(context.Context, agent.FrozenExecution) error{func(ctx context.Context, _ agent.FrozenExecution) error {
		select {
		case <-m.entered:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	w := newWorkflow(t, opts)
	diagnose := func() {
		s, err := w.Snapshot(t.Context())
		t.Logf("parallel stop: snapshotOK=%t state=%s code=%s stopped=%t revision=%d physical=%d requests=%d logical=%d effects=%d nodes=%d attempts=%d interactions=%d", err == nil, s.State, s.ErrorCode, s.ExecutionStopped, s.Revision, m.Calls(), s.Usage.TransportRequests, s.Usage.LogicalModelCalls, effects.Load(), len(s.WorkflowNodes), len(s.ModelAttempts), len(s.Interactions))
		for id, node := range s.WorkflowNodes {
			t.Logf("node=%s declared=%s kind=%s state=%s code=%s", id, node.NodeID, node.Kind, node.State, node.ErrorCode)
		}
		for id, attempt := range s.ModelAttempts {
			t.Logf("attempt=%s node=%s status=%s code=%s", id, attempt.NodeExecutionID, attempt.Status, attempt.FailureCode)
		}
		for id, interaction := range s.Interactions {
			t.Logf("interaction=%s node=%s state=%s", id, interaction.NodeExecutionID, interaction.State)
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		t.Logf("active=%t gateClosed=%t", w.active != nil, w.active != nil && w.active.gateClosed)
		for id, approval := range w.approvals {
			t.Logf("approval=%s requested=%t stopped=%t claimed=%t", id, approval.requestedExecution != "", approval.stoppedExecution != "", approval.claimedExecution != "")
		}
	}
	submit(t, w)
	end := time.Now().Add(5 * time.Second)
	waiting := false
	for time.Now().Before(end) {
		s, _ := w.Snapshot(t.Context())
		if s.State == "pausing" && m.Calls() == 1 && len(s.Interactions) == 1 {
			waiting = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !waiting {
		diagnose()
		t.Fatal("parallel tool did not request approval while model was executing")
	}
	release()
	s := waitStopped(t, w)
	if s.State != "failed" || s.ErrorCode != product.CodeInvalidArgument || s.FailedNode == "" || !s.ExecutionStopped || effects.Load() != 0 || m.Calls() != 1 || s.Usage.LogicalModelCalls != 1 || s.Usage.TransportRequests != 1 || s.Usage.ToolExecutions != 0 {
		diagnose()
		t.Errorf("approval hid model failure: state=%s code=%s model=%d tool=%d", s.State, s.ErrorCode, m.Calls(), effects.Load())
	}
	failed := s.WorkflowNodes[s.FailedNode]
	if failed.NodeID != "m" || failed.State != "failed" || failed.ErrorCode != product.CodeInvalidArgument || len(s.ModelAttempts) != 1 || len(s.Interactions) != 1 {
		diagnose()
		t.Error("terminal failure was not the real admitted invalid model")
	}
	for _, attempt := range s.ModelAttempts {
		if attempt.Status != "incomplete" || attempt.FailureCode != product.CodeInvalidArgument {
			t.Errorf("invalid model attempt status=%s code=%s", attempt.Status, attempt.FailureCode)
		}
	}
	for _, interaction := range s.Interactions {
		if interaction.State == "ready" || interaction.State == "pending" {
			t.Errorf("terminal run advertised an answerable interaction: %s", interaction.State)
		}
		_, err := w.RespondInteraction(t.Context(), WorkflowInteractionResponse{InteractionID: interaction.ID, Decision: "allowed-once", ExpectedRevision: s.Revision, IdempotencyKey: "terminal-answer", Principal: "local"})
		requireCode(t, err, product.CodeStateConflict)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, call := range w.state.Calls {
		if call.Claimed || call.Observation == nil || call.Observation.SideEffect != "none" || call.Observation.Executed {
			t.Errorf("terminal no-start proof: claimed=%t observed=%t", call.Claimed, call.Observation != nil)
		}
	}
}
