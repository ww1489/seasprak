package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestP2PrepareSchemaIntegerBoundsPreservePrecision(t *testing.T) {
	for _, tc := range []struct {
		name, constraint, value string
		valid                   bool
	}{
		{"minimum_below", `"minimum":9007199254740993`, "9007199254740992", false},
		{"minimum_equal", `"minimum":9007199254740993`, "9007199254740993", true},
		{"maximum_equal", `"maximum":9007199254740993`, "9007199254740993", true},
		{"maximum_above", `"maximum":9007199254740993`, "9007199254740994", false},
		{"maximum_rounded_up_above", `"maximum":9007199254740995`, "9007199254740996", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"n":` + tc.value + `}`
			sink := &recordSink{found: true, rec: accepted(input)}
			budget := agent.NewBudget(config.DefaultLimits())
			runs := 0
			def := addDef(func(_ context.Context, raw json.RawMessage) (string, error) {
				runs++
				if string(raw) != input {
					t.Errorf("arguments changed: %s", raw)
				}
				return "ok", nil
			})
			def.Schema = json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer",` + tc.constraint + `}},"required":["n"]}`)
			executor, err := NewExecutor("gen", []Definition{def}, sink, allow{}, budget)
			if err != nil {
				t.Fatal(err)
			}
			out, err := executor.Run(t.Context(), agent.ExecutionScope{SessionID: tc.name}, "prov-1", "add", input)
			// Invalid model arguments are a paired failed result, not a fatal
			// executor error. The preparation boundary retains invalid_argument.
			if err != nil {
				t.Fatal(err)
			}
			wantRuns, wantStatus := 0, "failed"
			if tc.valid {
				wantRuns, wantStatus = 1, "succeeded"
			}
			if runs != wantRuns || out.Status != wantStatus || out.Executed != tc.valid || budget.Snapshot().ToolExecutions != wantRuns {
				t.Errorf("out=%+v runs=%d usage=%+v; want status=%s runs=%d", out, runs, budget.Snapshot(), wantStatus, wantRuns)
			}
			assertPreparationFacts(t, sink, wantStatus, wantRuns)
			_, prepareErr := executor.prepareArguments(t.Context(), def, input)
			if tc.valid {
				if prepareErr != nil {
					t.Fatal(prepareErr)
				}
			} else if pe, ok := product.AsError(prepareErr); !ok || pe.Code != product.CodeInvalidArgument {
				t.Fatalf("preparation error=%v, want invalid_argument", prepareErr)
			}
		})
	}
}

func TestP2PrepareSchemaRejectsTrailingJSON(t *testing.T) {
	for _, raw := range []string{`{"type":"object"} {}`, `{"type":"object"} true`, `{"type":"object"} trailing`} {
		def := addDef(nil)
		def.Schema = json.RawMessage(raw)
		sink := &recordSink{}
		executor, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
		if pe, ok := product.AsError(err); executor != nil || !ok || pe.Code != product.CodeInvalidArgument || len(sink.facts) != 0 {
			t.Fatalf("trailing schema accepted: executor=%v error=%v facts=%d", executor, err, len(sink.facts))
		}
	}
}

func assertPreparationFacts(t *testing.T, sink *recordSink, status string, wantClaims int) {
	t.Helper()
	claims, observations := 0, 0
	for _, fact := range sink.facts {
		switch fact.Kind {
		case "tool_intent":
			claims++
		case "tool_observation":
			observations++
			var saved agent.ToolRecord
			if err := json.Unmarshal(fact.Payload, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Call != sink.rec.Call || saved.Scope != sink.rec.Scope || saved.Observation == nil || saved.Observation.Status != status || saved.Claimed != (wantClaims == 1) || saved.Observation.Executed != (wantClaims == 1) || saved.Observation.SideEffect != "none" {
				t.Errorf("unexpected paired observation: %+v observation=%+v", saved, saved.Observation)
			}
		}
	}
	if claims != wantClaims || observations != 1 {
		t.Errorf("claims=%d observations=%d; want claims=%d observations=1", claims, observations, wantClaims)
	}
}
