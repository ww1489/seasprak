package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestP2FrozenResolvesExecutionFromFinalArguments(t *testing.T) {
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	runs, resolves := 0, 0
	def := addDef(func(_ context.Context, raw json.RawMessage) (string, error) {
		runs++
		if string(raw) != `{"n":2}` {
			t.Fatalf("resolver changed execution arguments: %s", raw)
		}
		return "ok", nil
	})
	def.Execution.Resources = []agent.ExecutionResource{{Identity: "static", ExpectedVersion: "1"}}
	def.PrepareArguments = []func(context.Context, json.RawMessage) (json.RawMessage, error){func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"n":2}`), nil
	}}
	def.ResolveExecution = func(_ context.Context, raw json.RawMessage, d ExecutionDescription) (ExecutionDescription, error) {
		resolves++
		if string(raw) != `{"n":2}` || d.Resources[0].Identity != "static" {
			t.Fatal("resolver did not receive final parameters and an isolated static declaration")
		}
		d.Resources[0].Identity = "workspace/resource-2"
		d.EnvironmentRef = "protected:environment-2"
		raw[0] = '!'
		return d, nil
	}
	def.BeforeCall = []func(context.Context, agent.FrozenExecution) error{func(_ context.Context, f agent.FrozenExecution) error {
		if f.Resources[0].Identity != "workspace/resource-2" || f.EnvironmentRef != "protected:environment-2" || f.BackendID != "trusted-run" || string(f.FinalArguments) != `{"n":2}` {
			t.Fatal("hook did not see resolved frozen execution")
		}
		f.Resources[0].Identity = "hook mutation"
		return nil
	}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		out, err := exec.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
		if err != nil || !out.Executed {
			t.Fatalf("resolved execution failed: %+v %v", out, err)
		}
	}
	if runs != 2 || resolves != 2 || def.Execution.Resources[0].Identity != "static" {
		t.Fatal("resolution mutated the definition or did not run exactly once per call")
	}
	for _, fact := range sink.facts {
		if fact.Kind != "tool_frozen" {
			continue
		}
		var f agent.FrozenExecution
		if err := json.Unmarshal(fact.Payload, &f); err != nil {
			t.Fatal(err)
		}
		hash, err := f.Digest()
		if err != nil || f.Hash != hash || f.Resources[0].Identity != "workspace/resource-2" || string(f.FinalArguments) != `{"n":2}` {
			t.Fatal("resolved execution was not durably frozen before the hook")
		}
	}
}

func TestP2FrozenResolverFailurePreventsFreezeAndRun(t *testing.T) {
	for _, mode := range []string{"error", "panic", "invalid_limit", "invalid_arguments"} {
		t.Run(mode, func(t *testing.T) {
			args := `{"n":1}`
			if mode == "invalid_arguments" {
				args = `{"n":"bad"}`
			}
			sink := &recordSink{found: true, rec: accepted(args)}
			runs, resolves := 0, 0
			def := addDef(func(context.Context, json.RawMessage) (string, error) { runs++; return "ran", nil })
			def.ResolveExecution = func(_ context.Context, _ json.RawMessage, d ExecutionDescription) (ExecutionDescription, error) {
				resolves++
				switch mode {
				case "error":
					return d, errors.New("private resolver details")
				case "panic":
					panic("private resolver details")
				default:
					d.Timeout = -1
					return d, nil
				}
			}
			exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", args)
			if runs != 0 || out.Executed || hasIntent(sink) || !hasObservation(t, sink, "failed") {
				t.Fatal("failed resolution started an execution or lost its paired result")
			}
			for _, fact := range sink.facts {
				if fact.Kind == "tool_frozen" {
					t.Fatal("failed resolution was frozen")
				}
			}
			if mode == "invalid_arguments" && resolves != 0 {
				t.Fatal("resource resolution preceded final schema validation")
			}
			if mode == "error" || mode == "panic" {
				pe, ok := product.AsError(err)
				want := product.CodeResourceUnavailable
				if mode == "panic" {
					want = product.CodeInternal
				}
				if !ok || pe.Code != want || strings.Contains(err.Error(), "private resolver") {
					t.Fatalf("resolution failure code or redaction mismatch: %v", err)
				}
			}
		})
	}
}
