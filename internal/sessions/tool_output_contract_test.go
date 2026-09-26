package sessions

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/testkit"
)

type identityOutputProcess struct{ release <-chan struct{} }

func (p identityOutputProcess) Execute(ctx context.Context, request agent.AuthorizedProcess, output agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := request.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	if err := output.WriteProgress(ctx, agent.ProcessProgress{Text: "actual output"}); err != nil {
		return agent.ProcessObservation{Started: true}, err
	}
	select {
	case <-p.release:
		return agent.ProcessObservation{Started: true, Terminated: true, Content: "final result", SideEffect: "none"}, nil
	case <-ctx.Done():
		return agent.ProcessObservation{Started: true}, ctx.Err()
	}
}
func (identityOutputProcess) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}

// The public toolCallId identifies the product call, not the provider ID,
// which is only unique within a model turn and may be reused by the provider.
func TestToolOutputIdentifiesProductCall(t *testing.T) {
	release := make(chan struct{})
	model := testkit.NewFake(
		testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider-local-id", Name: "work", Arguments: `{}`}}},
		testkit.Step{Text: "done"},
	)
	def := tools.Definition{
		Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`),
		Execution: tools.ExecutionDescription{BackendID: "process-operations", Effect: "read", Argv: []string{"fixture"}},
	}
	s, err := CreateAgentSession(t.Context(), Options{SessionID: "output-identity", Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{def}, Operations: tools.Operations{Process: identityOutputProcess{release: release}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	defer close(release)
	sub := s.SubscribeEvents(config.DefaultLimits())
	defer sub.Close()
	if _, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"work"}`)}); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event, ok := <-sub.Events:
			if !ok {
				t.Fatalf("subscription ended before output: %v", sub.Err())
			}
			if event.Type != "tool.output.delta" {
				continue
			}
			var delta agent.ToolOutputDelta
			if err := json.Unmarshal(event.Payload, &delta); err != nil {
				t.Fatal(err)
			}
			view, err := s.Snapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(view.Calls) != 1 || model.Calls() != 1 {
				t.Fatalf("output did not precede final result: calls=%d model=%d", len(view.Calls), model.Calls())
			}
			for _, call := range view.Calls {
				if !call.Claimed || call.Observation != nil {
					t.Fatal("expected claimed call without final observation")
				}
				if delta.ToolCallID != call.Call.CallID || delta.ToolCallID == call.Call.ProviderCallID {
					t.Fatalf("toolCallId must reference product call: got=%q product=%q provider=%q", delta.ToolCallID, call.Call.CallID, call.Call.ProviderCallID)
				}
			}
			return
		case <-deadline.C:
			t.Fatal("tool output did not arrive while backend was blocked")
		}
	}
}
