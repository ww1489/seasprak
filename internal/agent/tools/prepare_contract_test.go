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
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestP2PrepareBusinessValidationSeesFinalCopy(t *testing.T) {
	const original, final = `{"n":1}`, `{"n":3}`
	sink := &recordSink{found: true, rec: accepted(original)}
	budget := agent.NewBudget(config.DefaultLimits())
	var order []string
	runs := 0
	def := addDef(func(_ context.Context, raw json.RawMessage) (string, error) {
		runs++
		order = append(order, "run")
		if string(raw) != final {
			t.Errorf("business validator mutated executed arguments: %s", raw)
		}
		return "ok", nil
	})
	def.PrepareArguments = []func(context.Context, json.RawMessage) (json.RawMessage, error){
		func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
			order = append(order, "first")
			if string(raw) != original {
				t.Errorf("first arguments=%s", raw)
			}
			return json.RawMessage(`{"n":2}`), nil
		},
		func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
			order = append(order, "last")
			if string(raw) != `{"n":2}` {
				t.Errorf("second arguments=%s", raw)
			}
			return json.RawMessage(final), nil
		},
	}
	def.Validate = func(_ context.Context, raw json.RawMessage) error {
		order = append(order, "validate")
		if string(raw) != final {
			t.Errorf("business validation preceded final transformation: %s", raw)
		}
		raw[5] = '9'
		return nil
	}
	def.BeforeCall = []func(context.Context, agent.FrozenExecution) error{func(_ context.Context, frozen agent.FrozenExecution) error {
		order = append(order, "hook")
		if string(frozen.FinalArguments) != final || frozen.FinalArgumentsHash != argumentHash([]byte(final)) {
			t.Error("validator mutation entered frozen execution")
		}
		return nil
	}}
	executor, err := NewExecutor("gen", []Definition{def}, sink, allow{}, budget)
	if err != nil {
		t.Fatal(err)
	}
	out, err := executor.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", original)
	if err != nil || out.Status != "succeeded" || runs != 1 || budget.Snapshot().ToolExecutions != 1 || strings.Join(order, ",") != "first,last,validate,hook,run" {
		t.Fatalf("out=%+v err=%v runs=%d order=%v usage=%+v", out, err, runs, order, budget.Snapshot())
	}
	assertPreparationFacts(t, sink, "succeeded", 1)
	frozenCount := 0
	for _, fact := range sink.facts {
		if fact.Kind != "tool_frozen" {
			continue
		}
		frozenCount++
		var frozen agent.FrozenExecution
		if err := json.Unmarshal(fact.Payload, &frozen); err != nil {
			t.Fatal(err)
		}
		if string(frozen.FinalArguments) != final || frozen.OriginalArgumentsHash != argumentHash([]byte(original)) {
			t.Fatal("validator mutation changed the durable description")
		}
	}
	if frozenCount != 1 {
		t.Fatalf("frozen facts=%d", frozenCount)
	}
}

func TestP2PrepareCallbackFailuresStopBeforeClaim(t *testing.T) {
	for _, stage := range []string{"prepare", "validate", "before_call"} {
		for _, panics := range []bool{false, true} {
			mode := "error"
			if panics {
				mode = "panic"
			}
			t.Run(stage+"/"+mode, func(t *testing.T) {
				sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
				budget := agent.NewBudget(config.DefaultLimits())
				callbacks, downstream, runs := 0, 0, 0
				fail := func() error {
					callbacks++
					if panics {
						panic("synthetic private callback detail")
					}
					return errors.New("synthetic private callback detail")
				}
				def := addDef(func(context.Context, json.RawMessage) (string, error) { runs++; return "unexpected", nil })
				def.BeforeCall = []func(context.Context, agent.FrozenExecution) error{func(context.Context, agent.FrozenExecution) error { downstream++; return nil }}
				switch stage {
				case "prepare":
					def.PrepareArguments = []func(context.Context, json.RawMessage) (json.RawMessage, error){func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, fail() }}
					def.Validate = func(context.Context, json.RawMessage) error { downstream++; return nil }
				case "validate":
					def.Validate = func(context.Context, json.RawMessage) error { return fail() }
				case "before_call":
					def.BeforeCall = append([]func(context.Context, agent.FrozenExecution) error{func(context.Context, agent.FrozenExecution) error { return fail() }}, def.BeforeCall...)
				}
				executor, err := NewExecutor("gen", []Definition{def}, sink, allow{}, budget)
				if err != nil {
					t.Fatal(err)
				}
				out, runErr := executor.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
				status, wantCode := "failed", product.CodeInvalidArgument
				if stage == "before_call" {
					status, wantCode = "denied", product.CodePermissionDenied
				}
				if panics {
					wantCode = product.CodeInternal
				}
				if callbacks != 1 || downstream != 0 || runs != 0 || out.Status != status || out.Executed || budget.Snapshot().ToolExecutions != 0 {
					t.Fatalf("callbacks=%d downstream=%d runs=%d out=%+v usage=%+v", callbacks, downstream, runs, out, budget.Snapshot())
				}
				assertPreparationFacts(t, sink, status, 0)
				boundaryErr := runErr
				if stage != "before_call" {
					if runErr != nil {
						t.Fatalf("ordinary argument failure must remain a paired result: %v", runErr)
					}
					// Check the internal error code separately without changing the
					// existing paired-result contract of Executor.Run.
					_, boundaryErr = executor.prepareArguments(t.Context(), def, `{"n":1}`)
				}
				if pe, ok := product.AsError(boundaryErr); !ok || pe.Code != wantCode {
					t.Fatalf("boundary error=%v want=%s", boundaryErr, wantCode)
				}
				raw, err := json.Marshal(sink.facts)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw)+out.Content+boundaryErr.Error(), "synthetic private") {
					t.Fatal("callback failure leaked private details")
				}
			})
		}
	}
}

func TestP2PrepareUncooperativeCallbacksWaitForExit(t *testing.T) {
	for _, stage := range []string{"prepare", "validate", "before_call"} {
		for _, returnContextError := range []bool{false, true} {
			mode := "nil"
			if returnContextError {
				mode = "context_error"
			}
			t.Run(stage+"/"+mode, func(t *testing.T) {
				entered := make(chan context.Context, 1)
				release := make(chan struct{})
				done := make(chan struct{})
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("executor did not exit after callback release")
					}
				}()
				var callbacks, downstream, runs atomic.Int32
				block := func(ctx context.Context) error {
					callbacks.Add(1)
					entered <- ctx
					<-release // Deliberately remains running beyond its deadline.
					if returnContextError {
						return ctx.Err()
					}
					return nil
				}
				def := addDef(func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "unexpected", nil })
				def.BeforeCall = []func(context.Context, agent.FrozenExecution) error{func(context.Context, agent.FrozenExecution) error { downstream.Add(1); return nil }}
				switch stage {
				case "prepare":
					def.PrepareArguments = []func(context.Context, json.RawMessage) (json.RawMessage, error){func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) { return raw, block(ctx) }}
					def.Validate = func(context.Context, json.RawMessage) error { downstream.Add(1); return nil }
				case "validate":
					def.Validate = func(ctx context.Context, _ json.RawMessage) error { return block(ctx) }
				case "before_call":
					def.BeforeCall = append([]func(context.Context, agent.FrozenExecution) error{func(ctx context.Context, _ agent.FrozenExecution) error { return block(ctx) }}, def.BeforeCall...)
				}
				limits := config.DefaultLimits()
				limits.HookTimeout = 20 * time.Millisecond
				budget := agent.NewBudget(limits)
				sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
				executor, err := NewExecutor("gen", []Definition{def}, sink, allow{}, budget)
				if err != nil {
					t.Fatal(err)
				}
				var out Outcome
				var runErr error
				go func() {
					out, runErr = executor.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
					close(done)
				}()
				var hookCtx context.Context
				select {
				case hookCtx = <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("callback did not enter")
				}
				select {
				case <-hookCtx.Done():
				case <-time.After(5 * time.Second):
					t.Fatal("hook deadline was not applied")
				}
				select {
				case <-done:
					t.Fatal("executor reported completion while callback was running")
				default:
				}
				if runs.Load() != 0 || downstream.Load() != 0 || budget.Snapshot().ToolExecutions != 0 {
					t.Fatal("execution progressed while callback was still running")
				}
				close(release)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("executor did not settle after callback exit")
				}
				if !errors.Is(runErr, context.DeadlineExceeded) || out.Status != "cancelled" || callbacks.Load() != 1 || downstream.Load() != 0 || runs.Load() != 0 || budget.Snapshot().ToolExecutions != 0 {
					t.Errorf("out=%+v error=%v callbacks=%d downstream=%d runs=%d usage=%+v", out, runErr, callbacks.Load(), downstream.Load(), runs.Load(), budget.Snapshot())
				}
				assertPreparationFacts(t, sink, "cancelled", 0)
			})
		}
	}
}
