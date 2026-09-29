package consumer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

type consumerTodoModel struct{ calls atomic.Int32 }

func (m *consumerTodoModel) Generate(context.Context, []*schema.AgenticMessage, ...einomodel.Option) (*schema.AgenticMessage, error) {
	n := m.calls.Add(1)
	if n%2 == 1 {
		return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "same-provider", Name: "write_todos", Arguments: `{"items":[{"title":"consumer task","state":"pending"}]}`})}, Extra: map[string]any{"seasprak.finish": "tool_calls"}}, nil
	}
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "done"})}, Extra: map[string]any{"seasprak.finish": "stop"}}, nil
}
func (m *consumerTodoModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	v, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{v}), nil
}

// This consumer verifies public execution/results and read-only disk reopen,
// not a public TODO-state query (which is intentionally not exposed).
func TestConsumerTodosPersistWithoutInjectedBackend(t *testing.T) {
	model := &consumerTodoModel{}
	opts := sdk.SessionOptions{SessionID: "consumer-todos", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sdk.ProfileMemory, Model: model, Tools: sdk.NewBuiltinDefinitions(sdk.BuiltinOptions{})}
	s, err := sdk.CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	var snapshot sdk.Snapshot
	for i := 0; i < 2; i++ {
		r, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"record task"}`)})
		if err != nil {
			t.Fatal(err)
		}
		snapshot = waitConsumerApprovalState(t, s, r.TraceID, "completed")
	}
	if len(snapshot.Calls) != 2 || model.calls.Load() != 4 {
		t.Fatal("unexpected TODO/model invocation counts")
	}
	invocations := map[string]bool{}
	for _, call := range snapshot.Calls {
		if !call.Claimed || call.Observation == nil || !call.Observation.Executed || call.Observation.Status != "succeeded" || call.Observation.SideEffect != "confirmed" || !json.Valid([]byte(call.Observation.Content)) {
			t.Fatal("public TODO result did not confirm a structured committed update")
		}
		invocations[call.Scope.InvocationID] = true
	}
	if len(invocations) != 2 {
		t.Fatal("TODO invocation identities were reused")
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opts.StateRoot, "sessions", opts.SessionID, "journal.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(before, []byte(`"type":"todo_update"`)) != 2 {
		t.Fatal("TODO results had no matching durable update facts")
	}
	opts.ReadOnly = true
	opened, err := sdk.OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close(context.Background())
	restored, err := opened.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.Calls, restored.Calls) || model.calls.Load() != 4 {
		t.Fatal("public reopen changed results or executed model")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only reopen modified journal", err)
	}
}
