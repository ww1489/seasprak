package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

type policyTestAuthorizer func(context.Context, agent.FrozenCall) (agent.Decision, error)

func (f policyTestAuthorizer) Authorize(ctx context.Context, c agent.FrozenCall) (agent.Decision, error) {
	return f(ctx, c)
}
func TestP2PolicyAuthorizerFailureKeepsPairAndCode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision agent.Decision
		err      error
		status   string
	}{
		{"permission", agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "denied"), "denied"},
		{"backend", agent.DecisionDeny, product.NewError(product.CodeResourceUnavailable, "backend unavailable"), "denied"},
		{"audit", agent.DecisionDeny, product.NewError(product.CodeStorageUnavailable, "audit unavailable"), "denied"},
		{"cancel-error", agent.DecisionCancel, context.Canceled, "cancelled"},
		{"cancel-decision", agent.DecisionCancel, nil, "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
			budget := agent.NewBudget(config.DefaultLimits())
			runs := 0
			exec, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) { runs++; return "ran", nil })}, sink, policyTestAuthorizer(func(context.Context, agent.FrozenCall) (agent.Decision, error) { return tc.decision, tc.err }), budget)
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
			if !errors.Is(err, tc.err) || out.Status != tc.status || out.Executed || runs != 0 || budget.Snapshot().ToolExecutions != 0 || hasIntent(sink) {
				t.Fatalf("err=%v out=%+v runs=%d", err, out, runs)
			}
			if pe, ok := product.AsError(tc.err); ok {
				actual, ok := product.AsError(err)
				if !ok || actual.Code != pe.Code {
					t.Fatal("public code was wrapped away")
				}
			}
			if len(sink.facts) != 2 || sink.facts[1].Kind != "tool_observation" {
				t.Fatal("missing paired observation")
			}
			var rec agent.ToolRecord
			_ = json.Unmarshal(sink.facts[1].Payload, &rec)
			if rec.Call != sink.rec.Call || rec.Claimed || rec.Observation == nil || rec.Observation.Executed || rec.Observation.Status != tc.status || rec.Observation.SideEffect != "none" {
				t.Fatalf("record=%+v", rec)
			}
		})
	}
}
