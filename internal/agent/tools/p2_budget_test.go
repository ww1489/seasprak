package tools

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"testing"
)

func TestP2BudgetClaimFailureHasZeroInvocationAndNoCacheChange(t *testing.T) {
	b := agent.NewBudget(config.DefaultLimits())
	disk := errors.New("disk")
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`), beforeCommit: func(f agent.Fact) error {
		if f.Kind == "tool_intent" {
			return disk
		}
		return nil
	}}
	calls := 0
	e, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) { calls++; return "ok", nil })}, sink, allow{}, b)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Run(context.Background(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
	if !errors.Is(err, disk) || calls != 0 || b.Snapshot().ToolExecutions != 0 || len(sink.facts) != 0 {
		t.Fatalf("err=%v calls=%d usage=%+v facts=%d", err, calls, b.Snapshot(), len(sink.facts))
	}
}

func TestP2BudgetCurrentEnvelopePreservesOriginalCallScope(t *testing.T) {
	original := agent.ExecutionScope{TraceID: "trace", InvocationID: "invocation", TurnID: "turn", ExecutionID: "old"}
	current := original
	current.ExecutionID = "new"
	current.TurnID = ""
	rec := accepted(`{"n":1}`)
	rec.Scope = original
	sink := &recordSink{found: true, rec: rec}
	b := agent.NewBudget(config.DefaultLimits())
	calls := 0
	e, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) { calls++; return "ok", nil })}, sink, allow{}, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Run(context.Background(), current, "prov-1", "add", `{"n":1}`); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || b.Snapshot().ToolExecutions != 1 {
		t.Fatal("wrong execution count")
	}
	for i, f := range sink.facts {
		if sink.scopes[i].ExecutionID != "new" {
			t.Fatal("old call scope replaced current envelope")
		}
		if f.Kind == "tool_observation" {
			var saved agent.ToolRecord
			if err := json.Unmarshal(f.Payload, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Scope != original {
				t.Fatal("original call scope rewritten")
			}
		}
	}
}
