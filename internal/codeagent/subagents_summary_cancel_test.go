package codeagent

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP3ChildSummaryCancelKeepsOriginalOverflowAndStopsRetry(t *testing.T) {
	var requests, effects atomic.Int32
	entered, exited := make(chan struct{}), make(chan struct{})
	child := p2FactoryModel(t, func(r *http.Request) (*http.Response, error) {
		switch n := requests.Add(1); n {
		case 1, 2, 3:
			return p3FactoryResponse(r, 200, p3FactoryAssistant("child step", &schema.FunctionToolCall{CallID: "probe-" + strconv.Itoa(int(n)), Name: "probe", Arguments: `{}`})), nil
		case 4:
			return p3FactoryResponse(r, 400, `{"error":{"code":"context_length_exceeded","message":"synthetic overflow"}}`), nil
		case 5:
			close(entered)
			<-r.Context().Done()
			close(exited)
			return nil, r.Context().Err()
		default:
			t.Error("cancelled summary retried the agent or started another summary")
			return nil, product.NewError(product.CodeInternal, "unexpected test request")
		}
	}, func(cfg *llm.ModelConfig) {
		// Reach the intended server overflow before the summary cancellation.
		cfg.Capabilities.ContextWindowTokens = 32768
		cfg.Capabilities.Items[llm.CapContextOverflow] = llm.Capability{Status: llm.Verified, AdapterVersion: "offline-child-fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline context_length_exceeded fixture"}}
	})
	main := testkit.NewFake(delegateCall("worker", "own task"), testkit.Step{Text: "parent must not continue"})
	opts := subagentOptions(agentRoots(t), "child-summary-cancel", main, []agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"probe"}}}, []tools.Definition{countedTool("probe", &effects, nil)})
	s := openSubagentSession(t, opts, true)
	in := submitPrompt(t, s, "delegate then cancel the auxiliary summary")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("actual summary request did not reach its cancellation boundary")
	}
	before := s.rt.manager.View()
	inv := onlyInvocation(t, before)
	originalCall, summaryID := "", ""
	for _, attempt := range childAttemptRecords(before, inv.ID) {
		if attempt.Purpose == "compaction" {
			summaryID = attempt.ID
		} else if result := before.AttemptResults[attempt.ID]; result.State == "failed" && before.AttemptDetails[result.DiagnosticRef].FailureReason == "context_overflow" {
			originalCall = attempt.ModelCallID
		}
	}
	if originalCall == "" || summaryID == "" || before.InvocationBudgets[inv.ID].Calls[originalCall].Requests != 1 || before.ModelAttempts[summaryID].ModelCallID == originalCall {
		t.Fatal("summary did not preserve the failed agent call before actual cancellation")
	}
	if err := s.Cancel(ctx, in.TraceID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("public cancellation returned before the summary wire exited")
	}
	v := s.rt.manager.View()
	inv = onlyInvocation(t, v)
	attempts := childAttemptRecords(v, inv.ID)
	budget := v.InvocationBudgets[inv.ID]
	result := v.AttemptResults[summaryID]
	details := v.AttemptDetails[result.DiagnosticRef]
	if requests.Load() != 5 || effects.Load() != 3 || main.Calls() != 1 || len(attempts) != 5 || inv.State != "cancelled" || inv.ModelCalls != 5 || len(inv.MessageIDs) != 3 || len(inv.CallIDs) != 3 || result.State != "aborted" || result.ExpectedRevision != 1 || result.Revision != 2 || details.FailureReason != "cancelled" || len(details.Usage) != 0 || budget.Usage.LogicalModelCalls != 5 || budget.Usage.TransportRequests != 5 || budget.Calls[originalCall].Requests != 1 || budget.Calls[v.ModelAttempts[summaryID].ModelCallID].Requests != 1 || v.Traces[in.TraceID].Usage.LogicalModelCalls != 6 || v.Traces[in.TraceID].Usage.TransportRequests != 6 || v.Traces[in.TraceID].State != "cancelled" || !v.Traces[in.TraceID].Settled {
		t.Fatalf("summary cancellation reset occupancy, accepted a candidate or continued work: requests=%d effects=%d attempts=%d invocation=%s calls=%d summary=%s", requests.Load(), effects.Load(), len(attempts), inv.State, inv.ModelCalls, result.State)
	}
	for _, msg := range v.Messages {
		if msg.Scope.InvocationID == inv.ID || msg.Kind == agent.KindCompactionSummary {
			t.Fatal("cancelled child summary became parent history")
		}
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	s = openSubagentSession(t, opts, false)
	after := s.rt.manager.View()
	if !reflect.DeepEqual(v.InvocationBudgets, after.InvocationBudgets) || !reflect.DeepEqual(v.AttemptResults, after.AttemptResults) || !reflect.DeepEqual(v.AttemptDetails, after.AttemptDetails) || after.Traces[in.TraceID].Usage != v.Traces[in.TraceID].Usage || requests.Load() != 5 || effects.Load() != 3 || main.Calls() != 1 {
		t.Fatal("reopen repeated cancelled summary/retry work or refunded reservations")
	}
}
