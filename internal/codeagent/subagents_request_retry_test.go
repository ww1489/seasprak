package codeagent

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

func p3FactoryResponse(r *http.Request, status int, body string) *http.Response {
	kind := "application/json"
	// Respond in the mode actually requested by the factory. Auxiliary calls
	// still use Generate; observed delegated agents use the parent's Stream rule.
	if status == http.StatusOK && r.GetBody != nil {
		request, err := r.GetBody()
		if err == nil {
			var mode struct {
				Stream bool `json:"stream"`
			}
			_ = json.NewDecoder(request).Decode(&mode)
			_ = request.Close()
			if mode.Stream {
				var response struct {
					Choices []struct {
						Index   int            `json:"index"`
						Message map[string]any `json:"message"`
						Finish  string         `json:"finish_reason"`
					} `json:"choices"`
					Usage json.RawMessage `json:"usage"`
				}
				if json.Unmarshal([]byte(body), &response) == nil {
					var choices []any
					for _, c := range response.Choices {
						if calls, ok := c.Message["tool_calls"].([]any); ok {
							for i, call := range calls {
								call.(map[string]any)["index"] = i
							}
						}
						choices = append(choices, map[string]any{"index": c.Index, "delta": c.Message, "finish_reason": c.Finish})
					}
					chunk, _ := json.Marshal(map[string]any{"choices": choices})
					usage, _ := json.Marshal(map[string]any{"choices": []any{}, "usage": response.Usage})
					body, kind = "data: "+string(chunk)+"\n\ndata: "+string(usage)+"\n\ndata: [DONE]\n\n", "text/event-stream"
				}
			}
		}
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func p3FactoryAssistant(text string, call *schema.FunctionToolCall) string {
	message := map[string]any{"role": "assistant", "content": text}
	finish := "stop"
	if call != nil {
		finish = "tool_calls"
		message["tool_calls"] = []any{map[string]any{"id": call.CallID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}}}
	}
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}})
	return string(raw)
}

func TestP3ChildFactoryRetryRetainsLogicalIdentityAndPerCallLimit(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "per_call_limit"}[limited], func(t *testing.T) {
			var requests, effects atomic.Int32
			child := p2FactoryModel(t, func(r *http.Request) (*http.Response, error) {
				switch n := requests.Add(1); n {
				case 1:
					return p3FactoryResponse(r, 503, `{"error":{"code":"service_error","message":"synthetic private retry diagnostic"}}`), nil
				case 2:
					return p3FactoryResponse(r, 200, p3FactoryAssistant("child accepted", &schema.FunctionToolCall{CallID: "original-child-tool", Name: "probe", Arguments: `{}`})), nil
				case 3:
					return p3FactoryResponse(r, 200, p3FactoryAssistant("child final", nil)), nil
				default:
					t.Error("unexpected extra child physical request")
					return nil, product.NewError(product.CodeInternal, "unexpected test request")
				}
			})
			main := testkit.NewFake(delegateCall("worker", "own task"), testkit.Step{Text: "parent final"})
			opts := subagentOptions(agentRoots(t), "child-factory-retry", main, []agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"probe"}}}, []tools.Definition{countedTool("probe", &effects, nil)})
			if limited {
				opts.Limits = config.Limits{LogicalModelRequests: 1}
			}
			s := openSubagentSession(t, opts, true)
			in := submitPrompt(t, s, "delegate with transient failure")
			waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
			v := s.rt.manager.View()
			inv := onlyInvocation(t, v)
			attempts := childAttemptRecords(v, inv.ID)
			wantRequests, wantCalls, wantEffects, wantState := int32(3), 2, int32(1), "completed"
			if limited {
				wantRequests, wantCalls, wantEffects, wantState = 1, 1, 0, "failed"
			}
			budget := v.InvocationBudgets[inv.ID]
			if requests.Load() != wantRequests || effects.Load() != wantEffects || len(attempts) != int(wantRequests) || inv.ModelCalls != wantCalls || inv.State != wantState || main.Calls() != 2 || v.Traces[in.TraceID].State != "completed" || budget.Usage.LogicalModelCalls != wantCalls || budget.Usage.TransportRequests != int(wantRequests) || v.Traces[in.TraceID].Usage.LogicalModelCalls != 2+wantCalls || v.Traces[in.TraceID].Usage.TransportRequests != 2+int(wantRequests) {
				t.Fatalf("retry changed logical/physical/effect counts: requests=%d effects=%d attempts=%d invocation=%s calls=%d", requests.Load(), effects.Load(), len(attempts), inv.State, inv.ModelCalls)
			}
			firstCall, failures, accepted := "", 0, 0
			for _, attempt := range attempts {
				result := v.AttemptResults[attempt.ID]
				if attempt.Purpose != "agent" || attempt.Scope.TurnID != "" || attempt.Scope.ParentInvocationID != inv.ParentInvocationID {
					t.Fatal("retry borrowed another invocation or parent Turn")
				}
				if result.State == "failed" {
					firstCall, failures = attempt.ModelCallID, failures+1
					if attempt.Attempt != 1 || v.AttemptDetails[result.DiagnosticRef].FailureReason != "service_unavailable" {
						t.Fatal("transient failure lost original attempt or trusted classification")
					}
				} else if result.State == "accepted" {
					accepted++
					usage := v.AttemptDetails[result.UsageRef].Usage
					if len(usage) != 1 || usage[0].Request.AttemptID != attempt.ID || usage[0].Request.TransportAttempt != 1 || !usage[0].Snapshot.Usage.InputTotal.Known || usage[0].Snapshot.Usage.InputTotal.Value != 7 {
						t.Fatal("retry evidence lost its own physical identity")
					}
				} else {
					t.Fatal("factory retry has no unique accepted or failed terminal")
				}
			}
			if failures != 1 || accepted != int(wantRequests)-1 || budget.Calls[firstCall].Requests != map[bool]int{false: 2, true: 1}[limited] {
				t.Fatal("retry created another logical call or reset its per-call limit")
			}
			if !limited {
				retried := 0
				for _, attempt := range attempts {
					if attempt.ModelCallID == firstCall && attempt.Attempt == 2 && v.AttemptResults[attempt.ID].State == "accepted" {
						retried++
					}
				}
				if retried != 1 || len(inv.CallIDs) != 1 {
					t.Fatal("successful retry did not reuse its original logical call and one tool")
				}
			}
			raw, _ := json.Marshal(v)
			if strings.Contains(string(raw), "synthetic private retry diagnostic") {
				t.Fatal("provider error text escaped the diagnostic allowlist")
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			s = openSubagentSession(t, opts, false)
			after := s.rt.manager.View()
			if !reflect.DeepEqual(v.InvocationBudgets, after.InvocationBudgets) || !reflect.DeepEqual(v.AttemptResults, after.AttemptResults) || after.Traces[in.TraceID].Usage != v.Traces[in.TraceID].Usage || requests.Load() != wantRequests || effects.Load() != wantEffects || main.Calls() != 2 {
				t.Fatal("reopen refunded retry occupancy or repeated known work")
			}
		})
	}
}

func TestP3ChildOverflowSummaryPreservesOriginalRetryBudget(t *testing.T) {
	var requests, effects atomic.Int32
	var s *AgentSession
	originalCall := ""
	child := p2FactoryModel(t, func(r *http.Request) (*http.Response, error) {
		n := requests.Add(1)
		scope := einorun.ScopeFromContext(r.Context(), agent.ExecutionScope{})
		budget := s.rt.manager.View().InvocationBudgets[scope.InvocationID]
		if budget.Scope != scope || budget.Usage.TransportRequests != int(n) || budget.Usage.LastTransport.AttemptID == "" {
			t.Error("physical child request preceded its independent durable reservation")
		}
		switch n {
		case 1, 2, 3:
			return p3FactoryResponse(r, 200, p3FactoryAssistant("child step", &schema.FunctionToolCall{CallID: "tool-" + string(rune('0'+n)), Name: "probe", Arguments: `{}`})), nil
		case 4:
			originalCall = budget.Usage.ModelCallID
			return p3FactoryResponse(r, 400, `{"error":{"code":"context_length_exceeded","message":"synthetic overflow"}}`), nil
		case 5:
			if budget.Usage.ModelCallID == originalCall || budget.Calls[originalCall].Requests != 1 {
				t.Error("summary replaced or advanced the failed agent logical call")
			}
			return p3FactoryResponse(r, 200, p3FactoryAssistant(summaryText(), nil)), nil
		case 6:
			body, err := io.ReadAll(r.Body)
			if err != nil || !strings.Contains(string(body), "## Goal") || budget.Usage.ModelCallID != originalCall || budget.Usage.ModelRequests != 2 || budget.Calls[originalCall].Requests != 2 {
				t.Error("overflow retry lost its original call, request limit or replacement projection")
			}
			return p3FactoryResponse(r, 200, p3FactoryAssistant("child final", nil)), nil
		default:
			t.Error("overflow or summary was automatically repeated")
			return nil, product.NewError(product.CodeInternal, "unexpected test request")
		}
	}, func(cfg *llm.ModelConfig) {
		// This fixture tests server overflow, not the automatic soft threshold.
		cfg.Capabilities.ContextWindowTokens = 32768
		cfg.Capabilities.Items[llm.CapContextOverflow] = llm.Capability{Status: llm.Verified, AdapterVersion: "offline-child-fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline context_length_exceeded fixture"}}
	})
	main := testkit.NewFake(delegateCall("worker", "own task"), testkit.Step{Text: "parent final"})
	opts := subagentOptions(agentRoots(t), "child-overflow-summary", main, []agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"probe"}}}, []tools.Definition{countedTool("probe", &effects, nil)})
	// The agent's failed request and successful retry fill exactly two slots.
	// The auxiliary summary must neither reset nor consume those slots.
	opts.Limits = config.Limits{LogicalModelRequests: 2}
	s = openSubagentSession(t, opts, true)
	in := submitPrompt(t, s, "delegate with certified overflow")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	v := s.rt.manager.View()
	inv := onlyInvocation(t, v)
	attempts := childAttemptRecords(v, inv.ID)
	if requests.Load() != 6 || effects.Load() != 3 || main.Calls() != 2 || inv.State != "completed" || inv.ModelCalls != 5 || len(attempts) != 6 || v.InvocationBudgets[inv.ID].Usage.LogicalModelCalls != 5 || v.InvocationBudgets[inv.ID].Usage.TransportRequests != 6 || v.Traces[in.TraceID].Usage.LogicalModelCalls != 7 || v.Traces[in.TraceID].Usage.TransportRequests != 8 {
		t.Fatalf("overflow summary lost or repeated work: requests=%d effects=%d attempts=%d invocation=%s calls=%d", requests.Load(), effects.Load(), len(attempts), inv.State, inv.ModelCalls)
	}
	summaries, failed, retried := 0, 0, 0
	for _, attempt := range attempts {
		result := v.AttemptResults[attempt.ID]
		if attempt.Purpose == "compaction" && result.State == "accepted" {
			summaries++
		}
		if attempt.ModelCallID == originalCall {
			if result.State == "failed" && attempt.Attempt == 1 && v.AttemptDetails[result.DiagnosticRef].FailureReason == "context_overflow" {
				failed++
			}
			if result.State == "accepted" && attempt.Attempt == 2 {
				retried++
			}
		}
	}
	if summaries != 1 || failed != 1 || retried != 1 {
		t.Fatal("summary or retry lost independent purpose and immutable attempt ordering")
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	s = openSubagentSession(t, opts, false)
	after := s.rt.manager.View()
	if !reflect.DeepEqual(v.InvocationBudgets, after.InvocationBudgets) || !reflect.DeepEqual(v.AttemptResults, after.AttemptResults) || requests.Load() != 6 || effects.Load() != 3 || main.Calls() != 2 {
		t.Fatal("reopen refunded or repeated interleaved overflow/summary work")
	}
}
