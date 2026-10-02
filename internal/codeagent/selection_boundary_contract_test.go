package codeagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

type selectionEffectiveModel struct {
	selectionConfiguredModel
	effective llm.EffectiveOptions
}

func (m selectionEffectiveModel) EffectiveOptions() llm.EffectiveOptions { return m.effective }

func TestP2ModelSwitchRevalidatesEffectiveOptions(t *testing.T) {
	for _, field := range []string{"output", "thinking", "cache"} {
		t.Run(field, func(t *testing.T) {
			f, release := runningSelectionFixture(t)
			defer release()
			selected := selectionEffectiveModel{selectionConfiguredModel: selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not execute"}), selectionTrustedConfig("selected")}}
			code := product.CodeUnsupportedCapability
			switch field {
			case "output":
				selected.effective.MaxOutputTokens = 1000
				code = product.CodeInvalidArgument
			case "thinking":
				selected.effective.RequestedThinking = "high"
			case "cache":
				selected.effective.RequestedCacheIntent = "invalid-intent"
				code = product.CodeInvalidArgument
			}
			before := f.manager.View()
			_, err := f.s.SelectNextTurnModel(t.Context(), SelectNextTurnModelRequest{TraceID: f.input.TraceID, Model: ModelChoice{Model: selected}, ExpectedRevision: before.LastSeq})
			requireSessionCode(t, err, code)
			if !reflect.DeepEqual(before, f.manager.View()) || selected.Calls() != 0 || f.runs.Load() != 0 {
				t.Fatal("invalid effective options changed selection or ran work")
			}
		})
	}
}

type selectionRequestTransport func(*http.Request) (*http.Response, error)

func (f selectionRequestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestP2ModelSwitchActualRequestOptionsCacheAndInventory(t *testing.T) {
	f, release := runningSelectionFixture(t)
	defer release()
	entered, gate := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	requests := make(chan map[string]any, 2)
	catalog := llm.NewCatalog(nil)
	err := catalog.RegisterOpenAIResponses(&http.Client{Transport: selectionRequestTransport(func(r *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return nil, err
		}
		requests <- payload
		output := `[{"id":"message","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"done","annotations":[]}]}]`
		if payload["model"] == "B" {
			close(entered)
			select {
			case <-gate:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			output = `[{"id":"function","type":"function_call","status":"completed","call_id":"unknown","name":"not_registered","arguments":"{}"}]`
		}
		body := `{"id":"response","object":"response","status":"completed","model":"` + payload["model"].(string) + `","output":` + output + `,"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	bound := map[string]model.AgenticModel{}
	for _, name := range []string{"B", "C"} {
		cfg := selectionTrustedConfig(name)
		cfg.Provider = "openai"
		cfg.Protocol = "openai-responses"
		cfg.Capabilities.Items[llm.CapCacheShort] = llm.Capability{Status: llm.Verified, AdapterVersion: "fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline selection contract"}}
		if err := catalog.Register(cfg); err != nil {
			t.Fatal(err)
		}
		limit := 64
		if name == "C" {
			limit = 96
		}
		model, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{CacheIntent: "short", MaxOutputTokens: limit})
		if err != nil {
			t.Fatal(err)
		}
		bound[name] = model
	}
	if _, err := f.s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: f.input.TraceID, ToolNames: []string{}, ExpectedRevision: f.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SelectNextTurnModel(t.Context(), SelectNextTurnModelRequest{TraceID: f.input.TraceID, Model: ModelChoice{Model: bound["B"]}, ExpectedRevision: f.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
	release()
	waitResumeCondition(t, func() bool {
		select {
		case <-entered:
			return true
		default:
			return terminal(f.manager.View().Traces[f.input.TraceID].State)
		}
	})
	select {
	case <-entered:
	default:
		t.Fatalf("B did not execute: %s", f.manager.View().Traces[f.input.TraceID].Error)
	}
	if _, err := f.s.SelectNextTurnModel(t.Context(), SelectNextTurnModelRequest{TraceID: f.input.TraceID, Model: ModelChoice{Model: bound["C"]}, ExpectedRevision: f.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: f.input.TraceID, ToolNames: []string{"work"}, ExpectedRevision: f.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
	close(gate)
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	view := f.manager.View()
	trace := view.Traces[f.input.TraceID]
	if trace.State != "completed" || len(requests) != 2 || f.model.Calls() != 1 || f.runs.Load() != 1 || trace.Usage.LogicalModelCalls != 3 || trace.Usage.TransportRequests != 3 || trace.Usage.ToolExecutions != 1 {
		t.Fatalf("trace=%+v requests=%d original=%d tools=%d", trace, len(requests), f.model.Calls(), f.runs.Load())
	}
	b, c := <-requests, <-requests
	if b["model"] != "B" || c["model"] != "C" || b["max_output_tokens"] != float64(64) || c["max_output_tokens"] != float64(96) {
		t.Fatal("selected model options were not used")
	}
	bTools, _ := b["tools"].([]any)
	cTools, _ := c["tools"].([]any)
	if len(bTools) != 0 || len(cTools) != 1 || cTools[0].(map[string]any)["name"] != "work" {
		t.Fatal("selected inventory not reflected in actual requests")
	}
	bk, _ := b["prompt_cache_key"].(string)
	ck, _ := c["prompt_cache_key"].(string)
	if len(bk) != 64 || len(ck) != 64 || bk == ck {
		t.Fatal("cache routing did not bind selected model/options")
	}
	for _, payload := range []map[string]any{b, c} {
		if payload["store"] != false || payload["previous_response_id"] != nil || payload["prompt_cache_retention"] != nil || len(payload["input"].([]any)) == 0 {
			t.Fatal("selection enabled stateful replay or lost full input")
		}
	}
}

func TestP2SelectionPendingRevokedBeforeActivation(t *testing.T) {
	gate := make(chan struct{})
	var runs atomic.Int32
	m := &hiddenSelectionModel{FakeModel: testkit.NewFake(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "bootstrap", Name: "unknown", Arguments: `{}`}}}, testkit.Step{Text: "must not execute"}), requests: make(chan hiddenSelectionRequest, 3)}
	s, err := CreateAgentSession(t.Context(), Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: m, Tools: []tools.Definition{{Name: "write", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "write"}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ok", nil }}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	input := submitOutput(t, s)
	hiddenSelectionNextRequest(t, m)
	receipt, err := s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: input.TraceID, ToolNames: []string{"write"}, ExpectedRevision: s.rt.manager.View().LastSeq})
	if err != nil {
		t.Fatal(err)
	}
	before := s.rt.manager.View()
	if err := s.rt.setExecutionPolicy(t.Context(), before.ExecutionPolicy.Revision, agent.ResolvedPolicy{SandboxMode: "read-only"}); err != nil {
		t.Fatal(err)
	}
	close(gate)
	waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[input.TraceID].State) })
	view := s.rt.manager.View()
	choice, _ := selectionForOperation(view, receipt.OperationID)
	trace := view.Traces[input.TraceID]
	if choice.State != "pending" || trace.State != "failed" || !strings.Contains(trace.Error, product.CodePermissionDenied) || len(view.Turns) != 1 || m.Calls() != 1 || runs.Load() != 0 || trace.Usage.ToolExecutions != 0 {
		t.Fatalf("revoked selection activated: state=%s choice=%s models=%d tools=%d", trace.State, choice.State, m.Calls(), runs.Load())
	}
}
