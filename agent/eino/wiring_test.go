package eino_test

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/agent"
	einorun "github.com/ww1489/seasprak/agent/eino"
	"github.com/ww1489/seasprak/internal/testkit"
	"github.com/ww1489/seasprak/model"
)

type nopSink struct{}

func (nopSink) CommitFact(context.Context, agent.ExecutionScope, agent.Fact) error { return nil }

func TestDeepAgentWiring(t *testing.T) {
	fake := testkit.NewFake(testkit.Step{Text: "wired"})
	ag, err := einorun.NewAgent(context.Background(), einorun.Deps{
		Model: fake, Sink: nopSink{}, Budget: agent.NewBudget(model.DefaultLimits()), Instruction: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag})
	iter := runner.Query(context.Background(), "ping")
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			t.Fatal(ev.Err)
		}
	}
	if fake.Calls() == 0 {
		t.Fatal("model was not called")
	}
}

func TestTurnLoopText(t *testing.T) {
	fake := testkit.NewFake(testkit.Step{Text: "wired"})
	ag, err := einorun.NewAgent(context.Background(), einorun.Deps{
		Model: fake, Sink: nopSink{}, Budget: agent.NewBudget(model.DefaultLimits()), Instruction: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	loop := adk.NewTurnLoop(adk.TurnLoopConfig[string, *schema.AgenticMessage]{
		GenInput: func(ctx context.Context, _ *adk.TurnLoop[string, *schema.AgenticMessage], items []string) (*adk.GenInputResult[string, *schema.AgenticMessage], error) {
			return &adk.GenInputResult[string, *schema.AgenticMessage]{
				Input:    &adk.TypedAgentInput[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{schema.UserAgenticMessage(items[0])}},
				Consumed: items[:1],
			}, nil
		},
		PrepareAgent: func(context.Context, *adk.TurnLoop[string, *schema.AgenticMessage], []string) (adk.TypedAgent[*schema.AgenticMessage], error) {
			return ag, nil
		},
		OnAgentEvents: func(_ context.Context, tc *adk.TurnContext[string, *schema.AgenticMessage], events *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]) error {
			for {
				ev, ok := events.Next()
				if !ok {
					break
				}
				if ev.Err != nil {
					return ev.Err
				}
			}
			tc.Loop.Stop(adk.WithImmediate())
			return nil
		},
	})
	loop.Push("ping")
	loop.Run(context.Background())
	done := make(chan struct{})
	go func() {
		_ = loop.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("turn loop did not finish")
	}
}
