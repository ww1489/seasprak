package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestToolOutputClosedBeforeObservation(t *testing.T) {
	for _, panics := range []bool{false, true} {
		name := "return"
		if panics {
			name = "panic"
		}
		t.Run(name, func(t *testing.T) {
			var escaped agent.ToolOutputSink
			var observationCount int
			sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
			sink.beforeCommit = func(fact agent.Fact) error {
				if fact.Kind == "tool_observation" {
					observationCount++
					before := len(sink.facts)
					err := escaped.WriteOutput(context.Background(), agent.ToolOutputChunk{Text: "late output"})
					if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict || len(sink.facts) != before {
						t.Errorf("output remained open during final observation: %v", err)
					}
				}
				return nil
			}
			def := addDef(nil)
			def.RunWithOutput = func(ctx context.Context, _ json.RawMessage, output agent.ToolOutputSink) (string, error) {
				escaped = output
				if err := output.WriteOutput(ctx, agent.ToolOutputChunk{Text: "actual output"}); err != nil {
					return "", err
				}
				if panics {
					panic("synthetic tool panic")
				}
				return "final", nil
			}
			exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithResourceScheduler(NewResourceScheduler()))
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := exec.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
			if err != nil || observationCount != 1 {
				t.Fatalf("final observation count=%d err=%v", observationCount, err)
			}
			if panics {
				if outcome.Status != "failed" || outcome.SideEffect != "unknown" || !outcome.Executed {
					t.Fatalf("panic was not conservatively settled: %+v", outcome)
				}
			} else if outcome.Status != "succeeded" || outcome.Content != "final" {
				t.Fatalf("final result changed: %+v", outcome)
			}
			if len(sink.facts) < 2 || sink.facts[len(sink.facts)-2].Kind != "tool_output" || sink.facts[len(sink.facts)-1].Kind != "tool_observation" {
				t.Fatal("output and final observation were committed in the wrong order")
			}
		})
	}
}
