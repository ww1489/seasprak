package codeagent

import (
	"net/http"
	"reflect"
	"strconv"
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

func TestP3ChildOverflowRecoveryBelongsToEachLogicalGeneration(t *testing.T) {
	for _, sameCall := range []bool{false, true} {
		t.Run(map[bool]string{false: "two_logical_calls", true: "same_call_second_overflow"}[sameCall], func(t *testing.T) {
			var requests, effects atomic.Int32
			var s *AgentSession
			var calls []string
			child := p2FactoryModel(t, func(r *http.Request) (*http.Response, error) {
				n := requests.Add(1)
				scope := einorun.ScopeFromContext(r.Context(), agent.ExecutionScope{})
				v := s.rt.manager.View()
				budget := v.InvocationBudgets[scope.InvocationID]
				attempt := v.ModelAttempts[budget.Usage.LastTransport.AttemptID]
				if budget.Scope != scope || scope.TurnID != "" || budget.Usage.TransportRequests != int(n) || attempt.State != "started" || attempt.ModelCallID != budget.Usage.ModelCallID {
					t.Error("child request preceded its own durable logical identity and attempt")
				}
				calls = append(calls, budget.Usage.ModelCallID)
				switch n {
				case 1, 2, 3:
					return p3FactoryResponse(r, 200, p3FactoryAssistant("child step", &schema.FunctionToolCall{CallID: "probe-" + strconv.Itoa(int(n)), Name: "probe", Arguments: `{}`})), nil
				case 4, 7:
					return p3FactoryResponse(r, 400, `{"error":{"code":"context_length_exceeded","message":"synthetic overflow"}}`), nil
				case 5, 8:
					original := calls[int(n)-2]
					if attempt.Purpose != "compaction" || budget.Usage.ModelCallID == original || budget.Calls[original].Requests != 1 {
						t.Error("summary changed the original agent call or its request count")
					}
					return p3FactoryResponse(r, 200, p3FactoryAssistant(summaryText(), nil)), nil
				case 6:
					if calls[5] != calls[3] || budget.Calls[calls[3]].Requests != 2 {
						t.Error("first recovery did not retry its original logical generation")
					}
					if sameCall {
						return p3FactoryResponse(r, 400, `{"error":{"code":"context_length_exceeded","message":"synthetic repeated overflow"}}`), nil
					}
					return p3FactoryResponse(r, 200, p3FactoryAssistant("child next step", &schema.FunctionToolCall{CallID: "probe-after-recovery", Name: "probe", Arguments: `{}`})), nil
				case 9:
					if calls[8] != calls[6] || calls[6] == calls[3] || budget.Calls[calls[6]].Requests != 2 {
						t.Error("second recovery reset or reused the wrong logical generation")
					}
					return p3FactoryResponse(r, 200, p3FactoryAssistant("child final", nil)), nil
				default:
					t.Error("overflow recovery exceeded its per-generation limit")
					return nil, product.NewError(product.CodeInternal, "unexpected test request")
				}
			}, func(cfg *llm.ModelConfig) {
				// Isolate the certified server overflow from soft compaction of
				// the larger, metadata-bearing streamed assistant messages.
				cfg.Capabilities.ContextWindowTokens = 32768
				cfg.Capabilities.Items[llm.CapContextOverflow] = llm.Capability{Status: llm.Verified, AdapterVersion: "offline-child-fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline context_length_exceeded fixture"}}
			})
			main := testkit.NewFake(delegateCall("worker", "own task"), testkit.Step{Text: "parent final"})
			opts := subagentOptions(agentRoots(t), "child-overflow-generations", main, []agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"probe"}}}, []tools.Definition{countedTool("probe", &effects, nil)})
			// The third request slot stays available: refusing a repeated overflow
			// must come from the recovery limit, not from exhausted request slots.
			opts.Limits = config.Limits{LogicalModelRequests: 3, OverflowRecoveries: 1}
			s = openSubagentSession(t, opts, true)
			in := submitPrompt(t, s, "delegate with two certified overflows")
			waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
			v := s.rt.manager.View()
			inv := onlyInvocation(t, v)
			wantRequests, wantCalls, wantEffects, wantSummaries, wantState := 9, 7, int32(4), 2, "completed"
			if sameCall {
				wantRequests, wantCalls, wantEffects, wantSummaries, wantState = 6, 5, 3, 1, "failed"
			}
			attempts := childAttemptRecords(v, inv.ID)
			budget := v.InvocationBudgets[inv.ID]
			if int(requests.Load()) != wantRequests || effects.Load() != wantEffects || len(attempts) != wantRequests || inv.State != wantState || inv.ModelCalls != wantCalls || main.Calls() != 2 || v.Traces[in.TraceID].State != "completed" || budget.Usage.LogicalModelCalls != wantCalls || budget.Usage.TransportRequests != wantRequests || v.Traces[in.TraceID].Usage.LogicalModelCalls != 2+wantCalls || v.Traces[in.TraceID].Usage.TransportRequests != 2+wantRequests || len(v.Turns) != 2 {
				t.Fatalf("overflow recovery crossed logical generations: requests=%d effects=%d attempts=%d invocation=%s logical=%d", requests.Load(), effects.Load(), len(attempts), inv.State, inv.ModelCalls)
			}
			summaries, failures := 0, 0
			for _, attempt := range attempts {
				result := v.AttemptResults[attempt.ID]
				if attempt.Scope.TurnID != "" || attempt.Scope.ParentInvocationID != inv.ParentInvocationID || result.Revision != 2 {
					t.Fatal("child recovery changed its original scope or unique terminal")
				}
				if attempt.Purpose == "compaction" && result.State == "accepted" {
					summaries++
				} else if attempt.Purpose == "agent" && result.State == "failed" && v.AttemptDetails[result.DiagnosticRef].FailureReason == "context_overflow" {
					failures++
				} else if result.State != "accepted" {
					t.Fatal("child overflow attempt lost trusted terminal classification")
				}
			}
			if summaries != wantSummaries || failures != 2 || budget.Calls[calls[3]].Requests != 2 || len(inv.CallIDs) != int(wantEffects) {
				t.Fatal("summary interleaving reset per-call recovery or repeated tool effects")
			}
			if sameCall {
				if out := delegateOutcome(t, delegateRecord(t, s)); out.Status != "failed" || out.Code != product.CodeResourceUnavailable {
					t.Fatalf("repeated overflow lost its public error code: %+v", out)
				}
			}
			for _, msg := range v.Messages {
				if msg.Scope.InvocationID == inv.ID || msg.Kind == agent.KindCompactionSummary {
					t.Fatal("child recovery modified parent history")
				}
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			s = openSubagentSession(t, opts, false)
			after := s.rt.manager.View()
			if !reflect.DeepEqual(v.InvocationBudgets, after.InvocationBudgets) || !reflect.DeepEqual(v.AttemptResults, after.AttemptResults) || after.Traces[in.TraceID].Usage != v.Traces[in.TraceID].Usage || int(requests.Load()) != wantRequests || effects.Load() != wantEffects || main.Calls() != 2 {
				t.Fatal("reopen refunded or repeated overflow recovery work")
			}
		})
	}
}
