package workflowagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

func parallelWorkflow() WorkflowDefinition {
	return WorkflowDefinition{Name: "parallel", Version: "v1", Source: "test", FormatVersion: WorkflowFormatV1, Resumable: true, InputSchema: json.RawMessage(`{"type":"object"}`), Nodes: []WorkflowNode{{ID: "s", Type: "start"}, {ID: "a", Type: "model", Model: "chosen", Prompt: "a"}, {ID: "b", Type: "model", Model: "chosen", Prompt: "b"}, {ID: "t", Type: "tool", Tool: "echo", Inputs: map[string]WorkflowValue{"q": {Literal: json.RawMessage(`"join"`)}}}, {ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"result": output("t", "result")}}}, Edges: []WorkflowEdge{{From: "s", To: "a"}, {From: "s", To: "b"}, {From: "a", To: "t"}, {From: "b", To: "t"}, {From: "t", To: "e"}}}
}
func TestWorkflowParallelLogicalIdentityAndJointBudget(t *testing.T) {
	for _, limit := range []int{2, 1} {
		t.Run(map[int]string{2: "full", 1: "limited"}[limit], func(t *testing.T) {
			var calls atomic.Int32
			m := testkit.NewFake(testkit.Step{Text: "a"}, testkit.Step{Text: "b"})
			opts := testOptions(t, parallelWorkflow(), m, &calls)
			opts.Limits = config.Limits{TraceLogicalModelCalls: limit}
			w := newWorkflow(t, opts)
			submit(t, w)
			s := waitStopped(t, w)
			w.mu.Lock()
			attempts := copyMap(w.state.Attempts)
			w.mu.Unlock()
			if s.Usage.LogicalModelCalls != limit || s.Usage.TransportRequests != limit || m.Calls() != limit {
				t.Fatalf("parallel budget %+v actual=%d", s.Usage, m.Calls())
			}
			ids := map[string]bool{}
			for _, a := range attempts {
				if a.Identity.ModelCallID != a.Scope.NodeExecutionID || ids[a.Identity.ModelCallID] || a.Scope.SessionID != "" || a.Scope.TurnID != "" {
					t.Fatalf("shared or fake identity %+v", a)
				}
				ids[a.Identity.ModelCallID] = true
			}
			if limit == 2 {
				if s.State != "completed" || calls.Load() != 1 || s.Usage.ToolExecutions != 1 {
					t.Fatalf("join %+v calls=%d", s, calls.Load())
				}
			} else if s.State != "failed" || s.ErrorCode != product.CodeBudgetExhausted || calls.Load() != 0 {
				t.Fatalf("limit %+v calls=%d", s, calls.Load())
			}
		})
	}
}

func TestWorkflowStaticSubflowHasIndependentInvocationAndPath(t *testing.T) {
	var count atomic.Int32
	sub := toolOnly()
	sub.Name = "child"
	d := sub
	d.Name = "parent"
	d.Nodes = []WorkflowNode{{ID: "s", Type: "start"}, {ID: "sub", Type: "subflow", Subflow: "child@v1"}, {ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"result": output("sub", "result")}}}
	d.Edges = []WorkflowEdge{{From: "s", To: "sub"}, {From: "sub", To: "e"}}
	opts := testOptions(t, d, nil, &count)
	opts.Subflows = map[string]WorkflowDefinition{"child@v1": sub}
	w := newWorkflow(t, opts)
	submit(t, w)
	s := waitStopped(t, w)
	if s.State != "completed" || count.Load() != 1 || s.Usage.ToolExecutions != 1 {
		t.Fatalf("subflow %+v count=%d", s, count.Load())
	}
	var outer, inner NodeRun
	for _, n := range s.WorkflowNodes {
		if n.Kind == "subflow" {
			outer = n
		} else if n.Kind == "tool" {
			inner = n
		}
	}
	if outer.ChildInvocationID == "" || inner.InvocationID != outer.ChildInvocationID || inner.InvocationID == outer.InvocationID || inner.Path != "sub/t" {
		t.Fatalf("subflow identities outer=%+v inner=%+v", outer, inner)
	}
}

type offlineWire func(*http.Request) (*http.Response, error)

func (f offlineWire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type observedModel struct {
	client  *http.Client
	entered atomic.Int32
}

func (*observedModel) Configuration() llm.ModelConfig { return llm.ModelConfig{Version: "observed-v1"} }
func (*observedModel) UsesObservedTransport() bool    { return true }
func (m *observedModel) Generate(ctx context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
	m.entered.Add(1)
	for i := 0; i < 2; i++ {
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/offline", nil)
		out, err := m.client.Do(r)
		if err != nil {
			return nil, err
		}
		io.Copy(io.Discard, out.Body)
		out.Body.Close()
	}
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "complete"})}, Extra: map[string]any{"seasprak.finish": "stop"}}, nil
}
func (m *observedModel) Stream(ctx context.Context, in []*schema.AgenticMessage, o ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, o...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}
func TestWorkflowObservedPhysicalReservationPrecedesOfflineWire(t *testing.T) {
	var tool atomic.Int32
	var physical atomic.Int32
	var w *WorkflowAgent
	var mu sync.Mutex
	m := &observedModel{}
	m.client = &http.Client{Transport: llm.NewObservedTransport(offlineWire(func(r *http.Request) (*http.Response, error) {
		n := physical.Add(1)
		w.mu.Lock()
		usage := w.state.Usage
		started := len(w.state.Attempts)
		w.mu.Unlock()
		if usage.TransportRequests < int(n) || started == 0 {
			t.Error("wire before durable occupancy/started")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":2,"completion_tokens":1}}`)), Request: r}, nil
	}), llm.UsageCollection{Protocol: "openai-chat", MaxBytes: 4096})}
	opts := testOptions(t, modelThenTool(), m, &tool)
	f := injectStore(t, &opts)
	w = newWorkflow(t, opts)
	f.reject = func(c storage.Commit) error {
		mu.Lock()
		defer mu.Unlock()
		for _, rec := range c.ControlRecords {
			if rec.Type == "workflow_budget" {
				var b budgetRecord
				json.Unmarshal(rec.Payload, &b)
				if b.Local.TransportRequests == 2 {
					return product.NewError(product.CodeStorageUnavailable, "synthetic second physical persist failure")
				}
			}
		}
		return nil
	}
	submit(t, w)
	waitExited(t, w)
	s, _ := w.Snapshot(t.Context())
	if m.entered.Load() != 1 || physical.Load() != 1 || tool.Load() != 0 || s.Usage.LogicalModelCalls != 1 || s.Usage.TransportRequests != 1 {
		t.Fatalf("requests entered=%d physical=%d tool=%d usage=%+v", m.entered.Load(), physical.Load(), tool.Load(), s.Usage)
	}
	mu.Lock()
	f.reject = nil
	mu.Unlock()
}
