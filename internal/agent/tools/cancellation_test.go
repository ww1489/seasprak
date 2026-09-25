package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
)

type cancelThenAllow struct{ cancel context.CancelFunc }

func (a cancelThenAllow) Authorize(context.Context, agent.FrozenCall) (agent.Decision, error) {
	a.cancel()
	return agent.DecisionAllow, nil
}

func reservedCall() agent.ToolRecord {
	rec := accepted(`{"n":1}`)
	rec.Call.Hash = "parent-hash"
	rec.Scope = agent.ExecutionScope{TraceID: "tr", TurnID: "turn-real"}
	return rec
}

func TestPersistOrAuthorizeCancelDoesNotStartTool(t *testing.T) {
	cases := []struct {
		name  string
		setup func(cancel context.CancelFunc, budg *agent.BudgetLedger, sink *recordSink) agent.ToolAuthorizer
	}{
		{
			name: "budget persist",
			setup: func(cancel context.CancelFunc, budg *agent.BudgetLedger, _ *recordSink) agent.ToolAuthorizer {
				budg.SetPersist(func(agent.Usage) error {
					cancel()
					return nil
				})
				return allow{}
			},
		},
		{
			name: "intent persist",
			setup: func(cancel context.CancelFunc, _ *agent.BudgetLedger, sink *recordSink) agent.ToolAuthorizer {
				sink.afterCommit = func(fact agent.Fact) {
					if fact.Kind == "tool_intent" {
						cancel()
					}
				}
				return allow{}
			},
		},
		{
			name: "authorize",
			setup: func(cancel context.CancelFunc, _ *agent.BudgetLedger, _ *recordSink) agent.ToolAuthorizer {
				return cancelThenAllow{cancel: cancel}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			rec := reservedCall()
			budg := agent.NewBudget(config.DefaultLimits())
			sink := &recordSink{found: true, rec: rec, budg: budg}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			auth := tc.setup(cancel, budg, sink)
			exec, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
				calls++
				return "1", nil
			})}, sink, auth, budg)
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Run(ctx, agent.ExecutionScope{TraceID: "tr"}, "prov-1", "add", `{"n":1}`)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err=%v out=%+v", err, out)
			}
			if calls != 0 || out.Executed || out.SideEffect != "none" || out.Status != "cancelled" {
				t.Fatalf("out=%+v calls=%d", out, calls)
			}
			if budg.Snapshot().ToolExecutions != 1 {
				t.Fatalf("budget=%+v", budg.Snapshot())
			}
			saved := mustObservation(t, sink)
			if !saved.Claimed || saved.Observation == nil || saved.Observation.Executed || saved.Observation.SideEffect != "none" || saved.Observation.Status != "cancelled" {
				t.Fatalf("observation %+v", saved)
			}
			if saved.Call.Hash != "parent-hash" || saved.Call.Name != "add" || saved.Call.Arguments != `{"n":1}` {
				t.Fatalf("frozen %+v", saved.Call)
			}
			if saved.Scope.TurnID != "turn-real" {
				t.Fatalf("scope %+v", saved.Scope)
			}
			if !hasIntent(sink) {
				t.Fatal("intent was not kept")
			}
		})
	}
}

func TestCancelObservationSaveFailureKeepsBothErrors(t *testing.T) {
	var calls int
	rec := reservedCall()
	budg := agent.NewBudget(config.DefaultLimits())
	saveErr := errors.New("disk")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &recordSink{
		found: true,
		rec:   rec,
		budg:  budg,
		afterCommit: func(fact agent.Fact) {
			if fact.Kind == "tool_intent" {
				cancel()
			}
		},
		beforeCommit: func(fact agent.Fact) error {
			if fact.Kind == "tool_observation" {
				return saveErr
			}
			return nil
		},
	}
	exec, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "1", nil
	})}, sink, allow{}, budg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(ctx, agent.ExecutionScope{TraceID: "tr"}, "prov-1", "add", `{"n":1}`)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, saveErr) {
		t.Fatalf("err=%v out=%+v", err, out)
	}
	if calls != 0 || out.Executed || out.SideEffect != "none" || out.Status != "cancelled" {
		t.Fatalf("out=%+v calls=%d", out, calls)
	}
	if budg.Snapshot().ToolExecutions != 1 {
		t.Fatalf("budget=%+v", budg.Snapshot())
	}
	if !hasIntent(sink) {
		t.Fatal("intent was not kept")
	}
	if hasObservationKind(sink) {
		t.Fatal("failed observation save was treated as saved")
	}
}

func mustObservation(t *testing.T, sink *recordSink) agent.ToolRecord {
	t.Helper()
	for i, fact := range sink.facts {
		if fact.Kind != "tool_observation" {
			continue
		}
		var saved agent.ToolRecord
		if err := json.Unmarshal(fact.Payload, &saved); err != nil {
			t.Fatal(err)
		}
		if sink.scopes[i].TurnID != "turn-real" {
			t.Fatalf("scope %+v", sink.scopes[i])
		}
		return saved
	}
	t.Fatal("missing observation")
	return agent.ToolRecord{}
}

func hasIntent(sink *recordSink) bool {
	for _, fact := range sink.facts {
		if fact.Kind == "tool_intent" {
			return true
		}
	}
	return false
}

func hasObservationKind(sink *recordSink) bool {
	for _, fact := range sink.facts {
		if fact.Kind == "tool_observation" {
			return true
		}
	}
	return false
}
