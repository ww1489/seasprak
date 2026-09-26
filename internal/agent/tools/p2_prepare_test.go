package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
)

func TestP2PrepareFinalValidationAndNumberPrecision(t *testing.T) {
	const large = `9007199254740993`
	for _, tc := range []struct {
		name, input, final string
		valid              bool
	}{
		{"last_conversion_invalid", `{"items":[1,2]}`, `{"items":[1,"bad"]}`, false},
		{"trailing_value", `{"items":[1,2]}`, `{"items":[1,2]} {}`, false},
		{"large_integer_and_array_order", `{"items":[1,2]}`, `{"items":[` + large + `,2]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordSink{found: true, rec: accepted(tc.input)}
			var starts atomic.Int32
			def := Definition{Name: "add", Version: "1", Schema: json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"integer"}}},"required":["items"]}`),
				PrepareArguments: []func(context.Context, json.RawMessage) (json.RawMessage, error){func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(tc.final), nil }},
				Run: func(_ context.Context, raw json.RawMessage) (string, error) {
					starts.Add(1)
					if string(raw) != tc.final {
						t.Errorf("final arguments changed: %s", raw)
					}
					return "ok", nil
				}}
			exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: tc.name}, "prov-1", "add", tc.input)
			if err != nil || (starts.Load() == 1) != tc.valid || out.Executed != tc.valid || hasIntent(sink) != tc.valid {
				t.Fatalf("out=%+v err=%v starts=%d", out, err, starts.Load())
			}
			if !tc.valid && !hasObservation(t, sink, "failed") {
				t.Fatal("invalid final parameters have no paired result")
			}
		})
	}
}

func TestP2PrepareHookDeadlineWaitsForRealExit(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	limits := config.DefaultLimits()
	limits.HookTimeout = 20 * time.Millisecond
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	var runs atomic.Int32
	def := addDef(func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "bad", nil })
	def.BeforeCall = []func(context.Context, agent.FrozenExecution) error{func(context.Context, agent.FrozenExecution) error {
		close(entered)
		<-release // An uncooperative trusted hook must actually exit first.
		return nil
	}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(limits))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var out Outcome
	var runErr error
	go func() {
		out, runErr = exec.Run(t.Context(), agent.ExecutionScope{SessionID: "hook-timeout"}, "prov-1", "add", `{"n":1}`)
		close(done)
	}()
	<-entered
	select {
	case <-done:
		t.Fatal("uncooperative hook was declared exited")
	case <-time.After(35 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("hook did not exit")
	}
	if !errors.Is(runErr, context.DeadlineExceeded) || runs.Load() != 0 || out.Executed || hasIntent(sink) || !hasObservation(t, sink, "cancelled") {
		t.Fatalf("out=%+v err=%v runs=%d", out, runErr, runs.Load())
	}
}

func TestP2FrozenStorageFailurePreventsHookAndStart(t *testing.T) {
	failure := errors.New("frozen append failed")
	var hooks, runs atomic.Int32
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`), beforeCommit: func(f agent.Fact) error {
		if f.Kind == "tool_frozen" {
			return failure
		}
		return nil
	}}
	def := addDef(func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "unexpected", nil })
	def.BeforeCall = []func(context.Context, agent.FrozenExecution) error{func(context.Context, agent.FrozenExecution) error { hooks.Add(1); return nil }}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "freeze-fault"}, "prov-1", "add", `{"n":1}`)
	if !errors.Is(err, failure) || hooks.Load() != 0 || runs.Load() != 0 || hasIntent(sink) || out.Executed || len(sink.facts) != 0 {
		t.Fatalf("out=%+v err=%v hooks=%d runs=%d", out, err, hooks.Load(), runs.Load())
	}
}

func TestP2PrepareHookCannotMutateFrozenArguments(t *testing.T) {
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	def := addDef(func(_ context.Context, raw json.RawMessage) (string, error) {
		if string(raw) != `{"n":1}` {
			t.Fatalf("hook changed run arguments: %s", raw)
		}
		return "ok", nil
	})
	def.BeforeCall = []func(context.Context, agent.FrozenExecution) error{func(_ context.Context, frozen agent.FrozenExecution) error {
		frozen.FinalArguments[5] = '9'
		frozen.Resources[0].Identity = "forged"
		return nil
	}}
	def.Execution.Resources = []agent.ExecutionResource{{Identity: "file"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "hook-copy"}, "prov-1", "add", `{"n":1}`)
	if err != nil || !out.Executed || !strings.Contains(out.Content, "ok") {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}
