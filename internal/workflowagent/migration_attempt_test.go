package workflowagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestWorkflowMigrationAttemptCommitFailuresKeepDurablePrefix(t *testing.T) {
	for _, stage := range []string{"logical", "started", "physical", "terminal"} {
		t.Run(stage, func(t *testing.T) {
			var effects, physical atomic.Int32
			m := migrationObservedModel(func(r *http.Request) (*http.Response, error) { physical.Add(1); return migrationResponse(r), nil })
			opts := testOptions(t, modelThenTool(), m, &effects)
			f := injectStore(t, &opts)
			w := newWorkflow(t, opts)
			var hit atomic.Bool
			f.reject = func(c storage.Commit) error {
				for _, rec := range c.ControlRecords {
					fail := false
					if rec.Type == "workflow_budget" {
						var b budgetRecord
						json.Unmarshal(rec.Payload, &b)
						fail = stage == "logical" && b.Local.LogicalModelCalls == 1 && b.Local.TransportRequests == 0 || stage == "physical" && b.Local.TransportRequests == 1
					}
					if rec.Type == "workflow_model_attempt" {
						var a attemptRecord
						json.Unmarshal(rec.Payload, &a)
						fail = stage == "started" && a.Status == "started" || stage == "terminal" && a.Status == "complete"
					}
					if fail {
						hit.Store(true)
						return product.NewError(product.CodeStorageUnavailable, "synthetic migration persist fault")
					}
				}
				return nil
			}
			submit(t, w)
			waitExited(t, w)
			if !hit.Load() {
				t.Fatal("real persistence boundary was not invoked")
			}
			w.mu.Lock()
			before := w.state.clone()
			broken := w.broken
			w.mu.Unlock()
			requireCode(t, broken, product.CodeStorageUnavailable)
			logical, requests, attempts, models := 0, 0, 0, int32(0)
			if stage != "logical" {
				logical = 1
			}
			if stage == "physical" || stage == "terminal" {
				attempts = 1
				models = 1
			}
			if stage == "terminal" {
				requests = 2
			}
			if before.Usage.LogicalModelCalls != logical || before.Usage.TransportRequests != requests || before.Usage.ToolExecutions != 0 || len(before.Attempts) != attempts || physical.Load() != int32(requests) || m.entered.Load() != models || effects.Load() != 0 {
				t.Fatal("fault advanced effects or lost durable reservations")
			}
			for _, a := range before.Attempts {
				if a.Status != "started" || a.Result != nil {
					t.Fatal("failed terminal append became visible")
				}
			}
			for _, n := range before.Nodes {
				if n.State == "completed" || n.Kind == "tool" {
					t.Fatal("uncommitted model result started successor")
				}
			}
			loaded, err := f.Load(t.Context(), opts.RunID)
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := replay(loaded, opts.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.Attempts, replayed.Attempts) || !reflect.DeepEqual(before.Nodes, replayed.Nodes) || !reflect.DeepEqual(before.Requests, replayed.Requests) || before.Usage != replayed.Usage || physical.Load() != int32(requests) || effects.Load() != 0 {
				t.Fatal("load-only replay changed prefix or executed work")
			}
			f.reject = nil
			if err = w.Close(context.Background()); err == nil {
				t.Fatal("Close concealed persistence fault")
			}
		})
	}
}

type migrationBlockedObservedModel struct {
	*observedModel
	blocked *testkit.FakeModel
}

func (m *migrationBlockedObservedModel) Generate(ctx context.Context, in []*schema.AgenticMessage, options ...model.Option) (*schema.AgenticMessage, error) {
	if _, err := m.observedModel.Generate(ctx, in, options...); err != nil {
		return nil, err
	}
	return m.blocked.Generate(ctx, in, options...)
}
func (m *migrationBlockedObservedModel) Stream(ctx context.Context, in []*schema.AgenticMessage, options ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, options...)
	if msg == nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), err
}

func TestWorkflowMigrationModelCancellationKeepsOriginalAbortedAttempt(t *testing.T) {
	var effects, physical atomic.Int32
	gate := make(chan struct{})
	defer close(gate)
	blocked := testkit.NewFake(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "late", Name: "echo", Arguments: `{}`}}})
	m := &migrationBlockedObservedModel{observedModel: migrationObservedModel(func(r *http.Request) (*http.Response, error) { physical.Add(1); return migrationResponse(r), nil }), blocked: blocked}
	opts := testOptions(t, modelThenTool(), m, &effects)
	w := newWorkflow(t, opts)
	submit(t, w)
	deadline := time.Now().Add(5 * time.Second)
	for blocked.Calls() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if blocked.Calls() != 1 {
		t.Fatal("model never actually invoked")
	}
	if _, err := w.Cancel(t.Context(), WorkflowControlCommand{Principal: "local"}); err != nil {
		t.Fatal(err)
	}
	s := waitStopped(t, w)
	w.mu.Lock()
	before := w.state.clone()
	w.mu.Unlock()
	if s.State != "cancelled" || !s.ExecutionStopped || effects.Load() != 0 || m.entered.Load() != 1 || blocked.Calls() != 1 || physical.Load() != 2 || len(before.Attempts) != 1 || before.Usage.LogicalModelCalls != 1 || before.Usage.TransportRequests != 2 || before.Usage.ToolExecutions != 0 || len(before.Nodes) != 1 {
		t.Fatal("cancel accepted response, refunded reservations, or started successor")
	}
	for _, a := range before.Attempts {
		if a.Status != "aborted" || a.Result == nil || len(a.Result.Details.Usage) != 2 || a.Identity.ModelCallID != a.Scope.NodeExecutionID || before.Nodes[a.Identity.ModelCallID].State == "completed" {
			t.Fatal("cancel lost unique original aborted terminal")
		}
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opts.ReadOnly = true
	r, err := OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	r.mu.Lock()
	after := r.state.clone()
	r.mu.Unlock()
	if !reflect.DeepEqual(before.Attempts, after.Attempts) || !reflect.DeepEqual(before.Requests, after.Requests) || before.Usage != after.Usage || physical.Load() != 2 || blocked.Calls() != 1 || effects.Load() != 0 {
		t.Fatal("read-only reopen restarted cancelled node")
	}
}

func TestWorkflowMigrationModelFailureKeepsPrivateTerminal(t *testing.T) {
	var effects atomic.Int32
	m := testkit.NewFake(testkit.Step{Text: "private candidate", Err: errors.New("synthetic private model failure")})
	w := newWorkflow(t, testOptions(t, modelThenTool(), m, &effects))
	submit(t, w)
	s := waitStopped(t, w)
	if s.State != "failed" || s.ErrorCode != product.CodeInternal || m.Calls() != 1 || effects.Load() != 0 || s.Usage.LogicalModelCalls != 1 || s.Usage.TransportRequests != 1 || len(s.WorkflowNodes) != 1 {
		t.Fatal("failed model advanced successor or lost code")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.state.Attempts) != 1 {
		t.Fatal("missing unique model terminal")
	}
	for _, a := range w.state.Attempts {
		if a.Status != "failed" || a.Result == nil {
			t.Fatalf("failed response lost private terminal: status=%s resultMissing=%t", a.Status, a.Result == nil)
		}
		if a.Result.Details.FailureCode != product.CodeResourceUnavailable || a.Result.Details.FailureReason != "model_error" {
			t.Fatal("model failure lost normalized private details")
		}
	}
}

func TestWorkflowMigrationTerminalCallCannotBeAcceptedAgain(t *testing.T) {
	var effects atomic.Int32
	w := newWorkflow(t, testOptions(t, toolOnly(), nil, &effects))
	submit(t, w)
	s := waitStopped(t, w)
	w.mu.Lock()
	var callID string
	for id := range w.state.Calls {
		callID = id
	}
	call := w.state.Calls[callID]
	w.mu.Unlock()
	_, err := w.LookupWorkflowTool(t.Context(), call.Scope, callID)
	requireCode(t, err, product.CodeStateConflict)
	after, _ := w.Snapshot(t.Context())
	if after.Revision != s.Revision || effects.Load() != 1 || after.Usage.ToolExecutions != 1 {
		t.Fatal("terminal call acceptance wrote or repeated effect")
	}
}
