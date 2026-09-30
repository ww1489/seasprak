package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
)

func TestTrustedCallbackCannotSuppressObservationWithInterruptionSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	calls := 0
	exec, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		calls++
		cancel()
		return "", ErrResumableInterruption
	})}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(ctx, agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
	if errors.Is(err, ErrResumableInterruption) || out.SideEffect != "unknown" || !out.Executed || !hasObservation(t, sink, "failed") || calls != 1 {
		t.Fatalf("callback suppressed durable effect: out=%+v err=%v calls=%d", out, err, calls)
	}
}
