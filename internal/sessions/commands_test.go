package sessions

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestExecuteCommandRejectsMissingControlledBackendBeforeStart(t *testing.T) {
	execute := builtinDefinitionForSession(t, "execute")
	s, err := CreateAgentSession(t.Context(), Options{Workspace: t.TempDir(), Profile: ProfileMemory, StateRoot: "memory", Model: testkit.NewFake(), Tools: []tools.Definition{execute}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	_, err = s.ExecuteCommand(t.Context(), CommandRequest{Name: "execute", Arguments: json.RawMessage(`{"argv":["echo","hi"]}`)})
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeResourceUnavailable {
		t.Fatalf("expected controlled backend rejection, got %v", err)
	}
	snap, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Operations) != 0 {
		t.Fatalf("missing backend created an operation: %+v", snap.Operations)
	}
}

func TestExecuteCommandPersistsCommandWithoutModelToolResult(t *testing.T) {
	process := &commandProcessProbe{}
	execute := builtinDefinitionForSession(t, "execute")
	s, err := CreateAgentSession(t.Context(), Options{Workspace: t.TempDir(), Profile: ProfileMemory, StateRoot: "memory", Model: testkit.NewFake(), Tools: []tools.Definition{execute}, Operations: tools.Operations{Process: process}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	receipt, err := s.ExecuteCommand(t.Context(), CommandRequest{IdempotencyKey: "command-once", Name: "execute", Arguments: json.RawMessage(`{"argv":["echo","hi"],"cwd":"workspace"}`)})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var snap Snapshot
	for time.Now().Before(deadline) {
		snap, err = s.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if snap.Operations[receipt.OperationID].State == "completed" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if process.calls.Load() != 1 || snap.Operations[receipt.OperationID].State != "completed" {
		t.Fatalf("command did not complete once: calls=%d op=%+v", process.calls.Load(), snap.Operations[receipt.OperationID])
	}
	var commandMessages, toolResults int
	for _, msg := range snap.Messages {
		if msg.Kind == agent.KindCommand {
			commandMessages++
		}
		if msg.Kind == agent.KindToolResult {
			toolResults++
		}
	}
	if commandMessages != 1 || toolResults != 0 {
		t.Fatalf("direct command projected wrong messages: command=%d toolResults=%d", commandMessages, toolResults)
	}
}

func builtinDefinitionForSession(t *testing.T, name string) tools.Definition {
	t.Helper()
	for _, def := range tools.NewBuiltinDefinitions(tools.BuiltinOptions{}) {
		if def.Name == name {
			return def
		}
	}
	t.Fatalf("builtin %q missing", name)
	return tools.Definition{}
}

type commandProcessProbe struct{ calls atomic.Int32 }

func (p *commandProcessProbe) Execute(ctx context.Context, req agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.calls.Add(1)
	return agent.ProcessObservation{Started: true, Terminated: true, ExitCode: 0, Content: "hi", SideEffect: "none"}, nil
}
func (*commandProcessProbe) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}
