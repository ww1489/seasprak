package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

func outputDefinition(run func(context.Context, json.RawMessage, agent.ToolOutputSink) (string, error)) Definition {
	def := addDef(nil)
	def.RunWithOutput = run
	return def
}

func outputFacts(t *testing.T, sink *recordSink) []agent.ToolOutputFact {
	t.Helper()
	var updates []agent.ToolOutputFact
	for _, fact := range sink.facts {
		if fact.Kind != "tool_output" {
			continue
		}
		var update agent.ToolOutputFact
		if err := json.Unmarshal(fact.Payload, &update); err != nil {
			t.Fatal(err)
		}
		updates = append(updates, update)
	}
	return updates
}

func TestRunWithOutputSplitsValidUTF8AndPreservesFinalResult(t *testing.T) {
	const arguments = `{"n":9007199254740993}`
	text := strings.Repeat("中文🚀", config.ToolOutputChunkBytes/6) + "尾"
	var escaped agent.ToolOutputSink
	sink := &recordSink{found: true, rec: accepted(arguments)}
	var invocations atomic.Int32
	exec, err := NewExecutor("gen", []Definition{outputDefinition(func(ctx context.Context, args json.RawMessage, output agent.ToolOutputSink) (string, error) {
		invocations.Add(1)
		if string(args) != arguments {
			t.Errorf("arguments changed: %s", args)
		}
		escaped = output
		if err := output.WriteOutput(context.Background(), agent.ToolOutputChunk{Text: text}); err != nil {
			return "", err
		}
		if err := output.WriteOutput(ctx, agent.ToolOutputChunk{Text: ""}); err != nil {
			return "", err
		}
		return "final", nil
	})}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", arguments)
	if err != nil || out.Content != "final" || out.Status != "succeeded" || invocations.Load() != 1 || !hasObservation(t, sink, "succeeded") {
		t.Fatalf("out=%+v err=%v invocations=%d", out, err, invocations.Load())
	}
	facts := outputFacts(t, sink)
	if len(facts) < 2 {
		t.Fatalf("output was truncated instead of split: %d", len(facts))
	}
	var joined string
	for i, fact := range facts {
		if fact.Stream != "output" || fact.CallID != "prod-1" || fact.ToolCallID != "prod-1" || fact.StreamID == "" || fact.StreamID != facts[0].StreamID || fact.ChunkSeq != uint64(i+1) || len(fact.Text) > config.ToolOutputChunkBytes || !utf8.ValidString(fact.Text) {
			t.Fatalf("chunk %d invalid: %+v", i, fact)
		}
		joined += fact.Text
	}
	if joined != text {
		t.Fatal("output text was lost or changed")
	}
	if err := escaped.WriteOutput(context.Background(), agent.ToolOutputChunk{Text: "late"}); err == nil {
		t.Fatal("closed callback accepted late output")
	}
	if len(outputFacts(t, sink)) != len(facts) {
		t.Fatal("late output was published")
	}
}

func TestRunWithOutputConcurrentWritersGetOneOrderedStream(t *testing.T) {
	const writers = 32
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	exec, err := NewExecutor("gen", []Definition{outputDefinition(func(ctx context.Context, _ json.RawMessage, output agent.ToolOutputSink) (string, error) {
		var group sync.WaitGroup
		for i := 0; i < writers; i++ {
			group.Add(1)
			go func() {
				defer group.Done()
				if err := output.WriteOutput(context.Background(), agent.ToolOutputChunk{Stream: "stderr", Text: "片段"}); err != nil {
					t.Errorf("write output: %v", err)
				}
			}()
		}
		group.Wait()
		return "done", nil
	})}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`); err != nil || out.Content != "done" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	facts := outputFacts(t, sink)
	if len(facts) != writers {
		t.Fatalf("wrote %d/%d", len(facts), writers)
	}
	for i, fact := range facts {
		if fact.ChunkSeq != uint64(i+1) || fact.StreamID != facts[0].StreamID || fact.Text != "片段" || fact.Stream != "stderr" {
			t.Fatalf("chunk[%d]=%+v", i, fact)
		}
	}
}

func TestRunWithOutputRejectsInvalidTextAndStreams(t *testing.T) {
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	exec, err := NewExecutor("gen", []Definition{outputDefinition(func(ctx context.Context, _ json.RawMessage, output agent.ToolOutputSink) (string, error) {
		for _, chunk := range []agent.ToolOutputChunk{{Stream: "stage", Text: "bad"}, {Text: string([]byte{0xff})}} {
			pe, ok := product.AsError(output.WriteOutput(ctx, chunk))
			if !ok || pe.Code != product.CodeInvalidArgument {
				t.Errorf("invalid output accepted: %+v err=%v", chunk, pe)
			}
		}
		return "done", nil
	})}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`); err != nil || len(outputFacts(t, sink)) != 0 {
		t.Fatalf("invalid output was published: %v", err)
	}
}

func TestRunWithOutputDeniedBudgetAndFailedClaimNeverInvokeOrEmit(t *testing.T) {
	for _, kind := range []string{"deny", "budget", "claim-fails", "reused", "claimed"} {
		t.Run(kind, func(t *testing.T) {
			var runs int
			def := outputDefinition(func(context.Context, json.RawMessage, agent.ToolOutputSink) (string, error) {
				runs++
				return "unexpected", nil
			})
			sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
			budget := agent.NewBudget(config.DefaultLimits())
			var auth agent.ToolAuthorizer = allow{}
			switch kind {
			case "deny":
				auth = deny{}
			case "budget":
				limits := config.DefaultLimits()
				limits.TraceToolCalls = 1
				budget = agent.NewBudget(limits)
				if err := budget.OccupyTool(); err != nil {
					t.Fatal(err)
				}
			case "claim-fails":
				sink.beforeCommit = func(fact agent.Fact) error {
					if fact.Kind == "tool_intent" {
						return errors.New("claim failed")
					}
					return nil
				}
			case "reused":
				sink.rec.Observation = &agent.ToolObservation{Status: "succeeded", Content: "cached"}
			case "claimed":
				sink.rec.Claimed = true
			}
			exec, err := NewExecutor("gen", []Definition{def}, sink, auth, budget)
			if err != nil {
				t.Fatal(err)
			}
			out, runErr := exec.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
			if kind == "claim-fails" && (runErr == nil || out.Executed) {
				t.Fatalf("claim failed but result=%+v err=%v", out, runErr)
			}
			if kind == "reused" && (out.Content != "cached" || runErr != nil) {
				t.Fatalf("cached=%+v err=%v", out, runErr)
			}
			if kind == "claimed" && runErr == nil {
				t.Fatal("claimed call was replayed")
			}
			if kind == "budget" && (runErr == nil || productCode(runErr) != product.CodeBudgetExhausted) {
				t.Fatalf("budget err=%v", runErr)
			}
			if runs != 0 || len(outputFacts(t, sink)) != 0 {
				t.Fatalf("runs=%d outputs=%d", runs, len(outputFacts(t, sink)))
			}
		})
	}
}
func productCode(err error) string {
	pe, _ := product.AsError(err)
	if pe == nil {
		return ""
	}
	return pe.Code
}

func TestRunWithOutputClosesBeforeFailedObservationAndDoesNotRerun(t *testing.T) {
	var escaped agent.ToolOutputSink
	var runs int
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	scope := agent.ExecutionScope{SessionID: "failed-save", TraceID: "trace", InvocationID: "invocation", TurnID: "turn"}
	sink.rec.Scope = scope
	scheduler := NewResourceScheduler()
	sink.beforeCommit = func(fact agent.Fact) error {
		if fact.Kind == "tool_observation" {
			return errors.New("failed saving observation")
		}
		return nil
	}
	exec, err := NewExecutor("gen", []Definition{outputDefinition(func(ctx context.Context, _ json.RawMessage, output agent.ToolOutputSink) (string, error) {
		runs++
		escaped = output
		if err := output.WriteOutput(ctx, agent.ToolOutputChunk{Text: "actual"}); err != nil {
			return "", err
		}
		return "final", nil
	})}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithResourceScheduler(scheduler), WithResourceDomain("memory", "failed-save"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = exec.Run(t.Context(), scope, "prov-1", "add", `{"n":1}`)
	if err == nil || runs != 1 || len(outputFacts(t, sink)) != 1 || hasObservation(t, sink, "succeeded") || !scheduler.HasHold(ResourceHoldID("failed-save", "prod-1")) {
		t.Fatalf("err=%v runs=%d facts=%d hold=%v", err, runs, len(sink.facts), scheduler.HasHold(ResourceHoldID("failed-save", "prod-1")))
	}
	if err := escaped.WriteOutput(context.Background(), agent.ToolOutputChunk{Text: "late"}); err == nil {
		t.Fatal("failed save reopened output")
	}
	// A committed claim exists even when the terminal observation failed.
	sink.rec.Claimed = true
	if _, err := exec.Run(t.Context(), scope, "prov-1", "add", `{"n":1}`); err == nil || runs != 1 || len(outputFacts(t, sink)) != 1 {
		t.Fatalf("replayed after failed save: %v runs=%d", err, runs)
	}
}

func TestRunWithOutputCannotUseBackgroundAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	exec, err := NewExecutor("gen", []Definition{outputDefinition(func(_ context.Context, _ json.RawMessage, output agent.ToolOutputSink) (string, error) {
		if err := output.WriteOutput(context.Background(), agent.ToolOutputChunk{Text: "before"}); err != nil {
			t.Error(err)
		}
		cancel()
		if err := output.WriteOutput(context.Background(), agent.ToolOutputChunk{Text: "after"}); !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled run accepted output: %v", err)
		}
		return "final", nil
	})}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = exec.Run(ctx, agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
	if got := outputFacts(t, sink); len(got) != 1 || got[0].Text != "before" {
		t.Fatalf("cancelled output=%+v", got)
	}
}

func TestRunWithOutputAndRunCannotCoexist(t *testing.T) {
	def := outputDefinition(func(context.Context, json.RawMessage, agent.ToolOutputSink) (string, error) { return "", nil })
	def.Run = func(context.Context, json.RawMessage) (string, error) { return "", nil }
	_, err := NewExecutor("gen", []Definition{def}, &recordSink{}, allow{}, agent.NewBudget(config.DefaultLimits()))
	if productCode(err) != product.CodeInvalidArgument {
		t.Fatalf("conflicting callbacks: %v", err)
	}
}
