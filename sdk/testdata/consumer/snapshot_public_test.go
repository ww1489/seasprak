package consumer_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

type consumerPrivateModel struct{ calls atomic.Int32 }

func (m *consumerPrivateModel) Generate(context.Context, []*schema.AgenticMessage, ...einomodel.Option) (*schema.AgenticMessage, error) {
	m.calls.Add(1)
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant,
		Extra: map[string]any{"seasprak.finish": "stop", "provider": "fixture-private-message"},
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.Reasoning{Text: "visible reasoning", Signature: "fixture-private-signature"}),
			schema.NewContentBlock(&schema.AssistantGenText{Text: "visible answer", Extension: map[string]any{"provider": "fixture-private-text"}}),
		},
	}, nil
}
func (m *consumerPrivateModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), err
}

func TestSDKConsumerSnapshotAndEventsShareRedactedMessages(t *testing.T) {
	model := &consumerPrivateModel{}
	opts := sdk.SessionOptions{SessionID: "consumer-public-projection", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sdk.ProfileMemory, Model: model}
	s, err := sdk.CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	sub := s.SubscribeEvents(sdk.DefaultLimits())
	t.Cleanup(sub.Close)
	input, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := waitConsumerApprovalState(t, s, input.TraceID, "completed")
	assertPublic := func(value any) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "fixture-private-") {
			t.Fatal("public view leaked provider replay metadata")
		}
	}
	assertPublic(snapshot)
	var assistant sdk.AgentMessage
	for _, message := range snapshot.Messages {
		if message.Kind == sdk.KindAssistant {
			assistant = message
		}
	}
	if assistant.Standard == nil || len(assistant.Standard.ContentBlocks) != 2 || assistant.Standard.ContentBlocks[0].Reasoning.Text != "visible reasoning" || assistant.Standard.ContentBlocks[1].AssistantGenText.Text != "visible answer" {
		t.Fatal("public projection lost visible content")
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	seen := false
	for !seen {
		select {
		case event, ok := <-sub.Events:
			if !ok {
				t.Fatalf("subscription closed: %v", sub.Err())
			}
			assertPublic(event)
			if event.Type == "message.finalized" {
				var message sdk.AgentMessage
				if err := json.Unmarshal(event.Payload, &message); err != nil {
					t.Fatal(err)
				}
				if message.Kind == sdk.KindAssistant {
					if !reflect.DeepEqual(message, assistant) {
						t.Fatal("snapshot and finalized event use different projections")
					}
					seen = true
				}
			}
		case <-timer.C:
			t.Fatal("missing finalized assistant event")
		}
	}
	assistant.Standard.ContentBlocks[0].Reasoning.Text = "mutated public copy"
	fresh, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range fresh.Messages {
		if message.Kind == sdk.KindAssistant && message.Standard.ContentBlocks[0].Reasoning.Text != "visible reasoning" {
			t.Fatal("public mutation changed committed state")
		}
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opts.ReadOnly, opts.Model = true, nil
	opened, err := sdk.OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	after, err := opened.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assertPublic(after)
	if after.Cursor != fresh.Cursor || after.Revision != fresh.Revision || !reflect.DeepEqual(after.Messages, fresh.Messages) || model.calls.Load() != 1 {
		t.Fatal("read-only replay changed messages, cursor or invocation count")
	}
}
