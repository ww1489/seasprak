package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestP2OriginUnavailableToolDoesNotRunHiddenDefinition(t *testing.T) {
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	runs := 0
	def := addDef(func(context.Context, json.RawMessage) (string, error) { runs++; return "ran", nil })
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.RejectUnavailable(t.Context(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
	if err != nil || out.Status != "denied" || out.Executed || runs != 0 || hasIntent(sink) || !hasObservation(t, sink, "denied") || len(sink.facts) != 1 {
		t.Fatalf("hidden definition escaped unavailable boundary: outcome=%+v err=%v facts=%d", out, err, len(sink.facts))
	}
}

func TestP2OriginUnavailableToolPropagatesObservationFailure(t *testing.T) {
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`), beforeCommit: func(fact agent.Fact) error {
		if fact.Kind != "tool_observation" {
			t.Error("unavailable tool attempted a non-observation commit")
		}
		return product.NewError(product.CodeStorageUnavailable, "observation rejected")
	}}
	exec, err := NewExecutor("gen", []Definition{addDef(nil)}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.RejectUnavailable(context.Background(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeStorageUnavailable || out.Executed || len(sink.facts) != 0 {
		t.Fatalf("denied observation persistence failure was lost: outcome=%+v err=%v", out, err)
	}
}
