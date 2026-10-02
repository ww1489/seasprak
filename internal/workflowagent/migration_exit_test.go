package workflowagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestWorkflowMigrationMissingInputAndFreeTextWriteNothing(t *testing.T) {
	var calls atomic.Int32
	m := testkit.NewFake()
	opts := testOptions(t, modelThenTool(), m, &calls)
	opts.Definition.InputSchema = json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
	w := newWorkflow(t, opts)
	before, _ := w.Snapshot(t.Context())
	for _, raw := range []string{`{}`, `"please summarize"`, `[]`, `{"name":7}`} {
		_, err := w.SubmitInput(t.Context(), WorkflowInputCommand{Input: json.RawMessage(raw), Principal: "local"})
		requireCode(t, err, product.CodeInvalidArgument)
	}
	after, _ := w.Snapshot(t.Context())
	if after.Revision != before.Revision || after.State != "created" || m.Calls() != 0 || calls.Load() != 0 || len(after.WorkflowNodes) != 0 {
		t.Fatal("invalid structured input wrote or started work")
	}
}

func TestWorkflowMigrationInvalidToolArgumentsStopDownstream(t *testing.T) {
	var calls atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &calls)
	// The first tool has invalid input; the valid second tool must never
	// be admitted or claimed even though it can otherwise run successfully.
	d := opts.Definition
	d.Nodes = []WorkflowNode{d.Nodes[0], d.Nodes[1], {ID: "later", Type: WorkflowNodeTool, Tool: "echo", Inputs: map[string]WorkflowValue{"q": {Literal: json.RawMessage(`1`)}}}, d.Nodes[2]}
	d.Nodes[3].Inputs = map[string]WorkflowValue{"result": output("later", "result")}
	d.Edges = []WorkflowEdge{{From: "s", To: "t"}, {From: "t", To: "later"}, {From: "later", To: "e"}}
	opts.Definition = d
	opts.Tools[0].Schema = json.RawMessage(`{"type":"object","properties":{"q":{"type":"integer"}}}`)
	w := newWorkflow(t, opts)
	submit(t, w)
	s := waitStopped(t, w)
	if s.State != "failed" || s.ErrorCode != product.CodeResourceUnavailable || calls.Load() != 0 || s.Usage.ToolExecutions != 0 || len(s.Result) != 0 || len(s.WorkflowNodes) != 1 {
		t.Fatalf("invalid args started tool: state=%s code=%s count=%d", s.State, s.ErrorCode, calls.Load())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.state.Calls {
		if c.Claimed || c.Observation == nil || c.Observation.SideEffect != "none" {
			t.Fatal("argument rejection lost no-start observation")
		}
	}
}

func TestWorkflowMigrationApprovalRejectsAndKeepsOriginalCall(t *testing.T) {
	var calls atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &calls)
	opts.Tools[0].Execution.RequestedGrantRef = "once"
	w := newWorkflow(t, opts)
	submit(t, w)
	s := waitStopped(t, w)
	var q WorkflowInteraction
	for _, q = range s.Interactions {
	}
	var original NodeRun
	for _, original = range s.WorkflowNodes {
	}
	_, err := w.RespondInteraction(t.Context(), WorkflowInteractionResponse{InteractionID: q.ID, Decision: "rejected", ExpectedRevision: s.Revision, Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	still, _ := w.Snapshot(t.Context())
	if still.State != "paused" || calls.Load() != 0 {
		t.Fatal("answer automatically executed")
	}
	_, err = w.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	s = waitStopped(t, w)
	if s.State != "failed" || s.ErrorCode != product.CodePermissionDenied || calls.Load() != 0 || s.Usage.ToolExecutions != 0 || len(s.WorkflowNodes) != 1 || s.WorkflowNodes[original.ID].ToolCallID != original.ToolCallID {
		t.Fatal("rejection ran, retried, or lost original call")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.state.Calls[original.ToolCallID]
	if len(w.state.Calls) != 1 || c.Claimed || c.Observation == nil || c.Observation.Status != "denied" || c.Observation.SideEffect != "none" {
		t.Fatal("rejected call lost no-effect proof")
	}
}

func TestWorkflowMigrationApprovalClaimsOriginalCallOnce(t *testing.T) {
	var effects atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &effects)
	opts.Tools[0].Execution.RequestedGrantRef = "once"
	w := newWorkflow(t, opts)
	submit(t, w)
	waiting := waitStopped(t, w)
	if waiting.State != "paused" || len(waiting.Interactions) != 1 || len(waiting.WorkflowNodes) != 1 || effects.Load() != 0 {
		t.Fatal("tool did not stop at original approval")
	}
	var question WorkflowInteraction
	for _, question = range waiting.Interactions {
	}
	original := waiting.WorkflowNodes[question.NodeExecutionID]
	w.mu.Lock()
	frozenHash := w.state.Frozen["execution:"+original.ToolCallID].Hash
	w.mu.Unlock()
	if original.ToolCallID == "" || original.ToolCallID != question.ToolCallID || frozenHash == "" {
		t.Fatal("approval lost original node/call binding")
	}
	_, err := w.RespondInteraction(t.Context(), WorkflowInteractionResponse{InteractionID: question.ID, Decision: "allowed-once", ExpectedRevision: waiting.Revision, Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	answered, _ := w.Snapshot(t.Context())
	if answered.Revision != waiting.Revision || effects.Load() != 0 {
		t.Fatal("instance answer wrote execution facts or auto-resumed")
	}
	cmd := WorkflowControlCommand{Principal: "local", IdempotencyKey: "resume-original"}
	receipt, err := w.Resume(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	final := waitStopped(t, w)
	w.mu.Lock()
	call := w.state.Calls[original.ToolCallID]
	unchangedHash := w.state.Frozen["execution:"+original.ToolCallID].Hash == frozenHash
	callCount := len(w.state.Calls)
	w.mu.Unlock()
	if final.State != "completed" || effects.Load() != 1 || final.Usage.ToolExecutions != 1 || final.Usage.LogicalModelCalls != 0 || len(final.WorkflowNodes) != 1 || final.WorkflowNodes[original.ID].ToolCallID != original.ToolCallID || callCount != 1 || !call.Claimed || call.Observation == nil || call.Observation.Status != "succeeded" || !unchangedHash {
		t.Fatal("approval replaced or executed the original call more than once")
	}
	duplicate, err := w.Resume(t.Context(), cmd)
	if err != nil || duplicate != receipt {
		t.Fatal("duplicate resume lost original receipt")
	}
	_, err = w.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
	requireCode(t, err, product.CodeIncompatibleResume)
	after, _ := w.Snapshot(t.Context())
	if after.Revision != final.Revision || effects.Load() != 1 || after.Usage != final.Usage {
		t.Fatal("terminal or duplicate resume wrote or repeated claim")
	}
}

func migrationObservedModel(wire func(*http.Request) (*http.Response, error)) *observedModel {
	m := &observedModel{}
	m.client = &http.Client{Transport: llm.NewObservedTransport(offlineWire(wire), llm.UsageCollection{Protocol: "openai-chat", MaxBytes: 4096})}
	return m
}
func migrationResponse(r *http.Request) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":7,"completion_tokens":3}}`)), Request: r}
}
func TestWorkflowMigrationParallelModelsKeepIndependentTwoRequestBudgets(t *testing.T) {
	var effects, physical atomic.Int32
	m := migrationObservedModel(func(r *http.Request) (*http.Response, error) { physical.Add(1); return migrationResponse(r), nil })
	w := newWorkflow(t, testOptions(t, parallelWorkflow(), m, &effects))
	submit(t, w)
	s := waitStopped(t, w)
	if s.State != "completed" || m.entered.Load() != 2 || physical.Load() != 4 || effects.Load() != 1 || s.Usage.LogicalModelCalls != 2 || s.Usage.TransportRequests != 4 {
		t.Fatal("parallel model totals or successor counts changed")
	}
	w.mu.Lock()
	before := w.state.clone()
	w.mu.Unlock()
	logical := map[string]bool{}
	for _, a := range before.Attempts {
		id := a.Identity.ModelCallID
		if logical[id] || id != a.Scope.NodeExecutionID || a.Status != "complete" || a.Result == nil || len(a.Result.Details.Usage) != 2 || a.Scope.SessionID != "" || a.Scope.TraceID != "" || a.Scope.TurnID != "" {
			t.Fatal("parallel nodes shared identity or lost terminal usage")
		}
		logical[id] = true
		if n := before.Nodes[id]; n.State != "completed" || n.Usage.LogicalModelCalls != 1 || n.Usage.TransportRequests != 2 || n.Usage.ModelRequests != 2 {
			t.Fatal("independent per-node occupancy lost")
		}
	}
	if len(logical) != 2 {
		t.Fatal("missing model attempts")
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opts := w.opts
	opts.ReadOnly = true
	r, err := OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	r.mu.Lock()
	after := r.state.clone()
	r.mu.Unlock()
	if !reflect.DeepEqual(before.Attempts, after.Attempts) || !reflect.DeepEqual(before.Requests, after.Requests) || before.Usage != after.Usage || physical.Load() != 4 || effects.Load() != 1 {
		t.Fatal("read-only reopen executed or changed reservations")
	}
}
func TestWorkflowMigrationPerCallPhysicalLimitStopsBeforeExcessWire(t *testing.T) {
	var effects, physical atomic.Int32
	m := migrationObservedModel(func(r *http.Request) (*http.Response, error) { physical.Add(1); return migrationResponse(r), nil })
	opts := testOptions(t, modelThenTool(), m, &effects)
	opts.Limits = config.Limits{LogicalModelRequests: 1}
	w := newWorkflow(t, opts)
	submit(t, w)
	s := waitStopped(t, w)
	if s.State != "failed" || s.ErrorCode != product.CodeBudgetExhausted || m.entered.Load() != 1 || physical.Load() != 1 || effects.Load() != 0 || s.Usage.LogicalModelCalls != 1 || s.Usage.TransportRequests != 1 || len(s.WorkflowNodes) != 1 {
		t.Fatal("per-call refusal started excess wire or successor")
	}
	w.mu.Lock()
	before := w.state.clone()
	w.mu.Unlock()
	for _, a := range before.Attempts {
		if a.Status != "failed" || a.Result == nil || a.Result.Details.FailureCode != product.CodeBudgetExhausted || len(a.Result.Details.Usage) != 1 || before.Nodes[a.Identity.ModelCallID].Usage.ModelRequests != 1 {
			t.Fatal("refusal lost terminal, original logical identity, or usage")
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
	if !reflect.DeepEqual(before.Attempts, after.Attempts) || !reflect.DeepEqual(before.Requests, after.Requests) || before.Usage != after.Usage || physical.Load() != 1 || effects.Load() != 0 {
		t.Fatal("reopen refunded or restarted refused node")
	}
}

func TestWorkflowMigrationWrappedErrorKeepsOnlyPublicCode(t *testing.T) {
	budget := product.NewError(product.CodeBudgetExhausted, "model request limit reached")
	for _, err := range []error{budget, &url.Error{Op: "private-operation", URL: "https://example.invalid/private", Err: budget}, errors.Join(errors.New("private diagnostic"), fmt.Errorf("private wrapper: %w", budget))} {
		if errorCode(err) != product.CodeBudgetExhausted {
			t.Fatal("wrapped public category lost")
		}
	}
	if errorCode(errors.New("private diagnostic")) != product.CodeInternal || errorCode(nil) != product.CodeInternal {
		t.Fatal("private diagnostic exposed")
	}
}

func TestWorkflowMigrationNodeAndCallCommitRejectProviderAndTurnIdentities(t *testing.T) {
	var effects atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &effects)
	f := injectStore(t, &opts)
	w := newWorkflow(t, opts)
	submit(t, w)
	waitStopped(t, w)
	loaded, err := f.Load(t.Context(), opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"provider", "turn", "call_missing"} {
		t.Run(field, func(t *testing.T) {
			forged := storage.CloneSession(loaded)
			for i := range forged.Commits {
				var kept []storage.Record
				for _, rec := range forged.Commits[i].ControlRecords {
					if rec.Type == "workflow_call" {
						var call agent.ToolRecord
						json.Unmarshal(rec.Payload, &call)
						switch field {
						case "provider":
							call.Call.ProviderCallID = "forged"
						case "turn":
							call.Scope.TurnID = "forged"
						case "call_missing":
							continue
						}
						rec = record(rec.Type, rec.ID, call)
					}
					kept = append(kept, rec)
				}
				forged.Commits[i].ControlRecords = kept
			}
			_, err := replay(forged, opts.RunID)
			requireCode(t, err, product.CodeIncompatibleVersion)
		})
	}
	if effects.Load() != 1 {
		t.Fatal("replay started effect")
	}
}
