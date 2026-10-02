package codeagent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/llm"
)

type privateReplayModel struct {
	calls   atomic.Int32
	streams atomic.Int32
}

func privateReplayFixture() *schema.AgenticMessage {
	opaque := schema.NewContentBlock(&schema.Reasoning{})
	opaque.Extra = map[string]any{"_eino_ext_agentic_claude_redacted_thinking": "synthetic-private-opaque", "other-provider": map[string]any{"data": "synthetic-private-block"}}
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant,
		Extra:         map[string]any{"seasprak.finish": "stop", "provider": "synthetic-private-message"},
		ResponseMeta:  &schema.AgenticResponseMeta{TokenUsage: &schema.TokenUsage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}, Extension: map[string]any{"data": "synthetic-private-meta"}},
		ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.Reasoning{Text: "visible reasoning", Signature: "synthetic-private-signature"}), opaque, schema.NewContentBlock(&schema.AssistantGenText{Text: "visible answer", Extension: map[string]any{"data": "synthetic-private-text"}})},
	}
}
func (m *privateReplayModel) Generate(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
	m.calls.Add(1)
	return privateReplayFixture(), nil
}
func (m *privateReplayModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.streams.Add(1)
	msg, err := m.Generate(ctx, in, opts...)
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), err
}
func assertNoPrivateReplay(t *testing.T, label string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "synthetic-private-") {
		t.Errorf("%s exposes private replay material", label)
	}
}
func assertPrivateReplay(t *testing.T, s *AgentSession) {
	t.Helper()
	msgs, err := agent.ConvertToLLM(s.rt.manager.View().Messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || !reflect.DeepEqual(msgs[1], privateReplayFixture()) {
		t.Error("internal replay lost private data, empty reasoning, metadata or block order")
	}
}
func TestSessionPublicMessageProjectionPreservesPrivateReplay(t *testing.T) {
	m := &privateReplayModel{}
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: llm.WithObservedTransportModel(m), GenerationFingerprint: "private-replay-v1"}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	sub := s.SubscribeEvents(config.DefaultLimits())
	defer sub.Close()
	receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[receipt.TraceID].State) })
	if s.rt.manager.View().Traces[receipt.TraceID].State != "completed" || m.calls.Load() != 1 || m.streams.Load() != 1 {
		t.Fatal("expected exactly one successful streamed model call")
	}
	snap, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assertNoPrivateReplay(t, "snapshot", snap)
	if len(snap.Messages) != 2 || snap.Messages[1].Standard == nil || len(snap.Messages[1].Standard.ContentBlocks) != 3 {
		t.Fatal("public snapshot lost the accepted message blocks")
	}
	public := snap.Messages[1].Standard
	if public.ContentBlocks[0].Reasoning.Text != "visible reasoning" || public.ContentBlocks[1].Reasoning == nil || public.ContentBlocks[2].AssistantGenText.Text != "visible answer" || public.ResponseMeta.TokenUsage.TotalTokens != 7 {
		t.Fatal("public snapshot lost visible content, empty reasoning or usage")
	}
	assertPrivateReplay(t, s)
	finalCount, streamCount := 0, 0
	durable := map[string]agent.Event{}
	for _, ev := range s.rt.manager.View().Events {
		durable[ev.EventID] = ev
		assertNoPrivateReplay(t, "durable event", ev)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case ev, ok := <-sub.Events:
			if !ok {
				t.Fatalf("subscription closed: %v", sub.Err())
			}
			assertNoPrivateReplay(t, "subscribed event", ev)
			if ev.DurableSeq != nil && !reflect.DeepEqual(ev, durable[ev.EventID]) {
				t.Error("subscribed durable event differs from journal")
			}
			if ev.Type == "message.snapshot" {
				streamCount++
				var update agent.ModelStreamSnapshot
				if err := json.Unmarshal(ev.Payload, &update); err != nil {
					t.Fatal(err)
				}
				if len(update.Blocks) != 3 || update.Blocks[0].Text != "visible reasoning" || update.Blocks[1].Text != "" || update.Blocks[2].Text != "visible answer" {
					t.Error("incremental projection lost visible content or block order")
				}
			}
			if ev.Type == "message.finalized" {
				var msg agent.AgentMessage
				if err := json.Unmarshal(ev.Payload, &msg); err != nil {
					t.Fatal(err)
				}
				if msg.Kind == agent.KindAssistant {
					finalCount++
					if !reflect.DeepEqual(msg, snap.Messages[1]) {
						t.Error("finalized message differs from snapshot")
					}
				}
			}
			if ev.DurableSeq != nil && *ev.DurableSeq == snap.Cursor {
				goto received
			}
		case <-deadline.C:
			t.Fatal("subscription did not deliver through snapshot cursor")
		}
	}
received:
	if finalCount != 1 || streamCount == 0 {
		t.Fatalf("final=%d snapshots=%d", finalCount, streamCount)
	}
	// Public mutation must not change subsequent snapshots or private replay.
	snap.Messages[1].Standard.ContentBlocks[0].Reasoning.Text = "mutated"
	assertPrivateReplay(t, s)
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	after, err := reopened.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assertNoPrivateReplay(t, "reopened snapshot", after)
	assertPrivateReplay(t, reopened)
	if after.Cursor != snap.Cursor || m.calls.Load() != 1 {
		t.Error("read-only reopen changed cursor or executed model")
	}
	for _, ev := range reopened.rt.manager.View().Events {
		assertNoPrivateReplay(t, "replayed durable event", ev)
		if !reflect.DeepEqual(ev, durable[ev.EventID]) {
			t.Error("replay changed durable event identity or body")
		}
	}
}
