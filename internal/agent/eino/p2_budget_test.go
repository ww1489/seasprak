package eino

import (
	"context"
	"errors"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
	"testing"
)

func TestP2BudgetModelCancellationDuringCommitStartsNoRequest(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "generate", true: "stream"}[stream], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := agent.NewBudget(config.DefaultLimits())
			b.SetPersist(func(u agent.Usage) error {
				if u.TransportRequests == 1 {
					cancel()
				}
				return nil
			})
			fake := testkit.NewFake(testkit.Step{Text: "ok"})
			vm := NewValidatedModel(fake, &factSink{}, b, agent.ExecutionScope{})
			var err error
			if stream {
				_, err = vm.Stream(ctx, nil)
			} else {
				_, err = vm.Generate(ctx, nil)
			}
			if !errors.Is(err, context.Canceled) || fake.Calls() != 0 || b.Snapshot().TransportRequests != 1 {
				t.Fatalf("err=%v calls=%d usage=%+v", err, fake.Calls(), b.Snapshot())
			}
		})
	}
}

func TestP2BudgetRestoredModelCannotStartFourthRequest(t *testing.T) {
	fake := testkit.NewFake(testkit.Step{Text: "ok", Repeat: true})
	b := agent.NewBudget(config.Limits{LogicalModelRequests: 3})
	scope := agent.ExecutionScope{TurnID: "logical"}
	for i := 0; i < 3; i++ {
		vm := NewValidatedModel(fake, &factSink{}, b, scope)
		if _, err := vm.Generate(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		next := agent.NewBudget(config.Limits{LogicalModelRequests: 3})
		next.Restore(b.Snapshot())
		b = next
	}
	_, err := NewValidatedModel(fake, &factSink{}, b, scope).Generate(context.Background(), nil)
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeBudgetExhausted || fake.Calls() != 3 || b.Snapshot().LogicalModelCalls != 1 {
		t.Fatalf("err=%v calls=%d usage=%+v", err, fake.Calls(), b.Snapshot())
	}
}
