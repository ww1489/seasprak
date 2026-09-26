package sessions_test

import (
	"context"
	"errors"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestSessionCanQueryAcceptedOperation(t *testing.T) {
	s, err := sessions.CreateAgentSession(t.Context(), sessions.Options{SessionID: "operation-query", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sessions.ProfileMemory, Model: testkit.NewFake()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	reader, ok := any(s).(interface {
		GetOperation(context.Context, string) (state.OperationStatus, error)
	})
	if !ok {
		t.Fatal("session cannot query a durable accepted operation by its receipt ID")
	}
	if _, err := reader.GetOperation(t.Context(), "missing"); err == nil {
		t.Fatal("missing operation returned success")
	} else if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeNotFound {
		t.Fatalf("missing operation code: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.GetOperation(ctx, "missing"); !errors.Is(err, context.Canceled) {
		t.Fatalf("query ignored caller cancellation: %v", err)
	}
}
