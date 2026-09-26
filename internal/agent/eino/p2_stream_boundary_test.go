package eino

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
)

type p2BoundaryStream struct {
	beforeEnd func()
	requests  int
}

func (m *p2BoundaryStream) Generate(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
	panic("stream required")
}
func (m *p2BoundaryStream) Stream(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.requests++
	i := 0
	return schema.StreamReaderWithConvert(schema.StreamReaderFromArray([]int{0, 1}), func(int) (*schema.AgenticMessage, error) {
		defer func() { i++ }()
		if i == 0 {
			return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "partial"}), schema.NewContentBlock(&schema.Reasoning{Text: "public", Signature: "synthetic-private-signature"})}, Extra: map[string]any{"private": "synthetic-private-extra"}}, nil
		}
		m.beforeEnd()
		return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, Extra: map[string]any{"seasprak.finish": "stop"}}, nil
	}), nil
}
func TestStreamBoundaryPreservesProviderBlockIndex(t *testing.T) {
	msg := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlockChunk(&schema.AssistantGenText{Text: "second"}, &schema.StreamingMeta{Index: 7})}, Extra: map[string]any{"seasprak.finish": "stop"}}
	sink := &factSink{}
	vm := NewValidatedModel(malformedModel{msg: msg}, sink, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{})
	r, err := vm.Stream(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	for _, fact := range sink.facts {
		if fact.Kind != "model_stream_snapshot" {
			continue
		}
		var update agent.ModelStreamSnapshot
		if err := json.Unmarshal(fact.Payload, &update); err != nil {
			t.Fatal(err)
		}
		if len(update.Blocks) != 1 || update.Blocks[0].BlockIndex != 7 {
			t.Fatal("temporary snapshot renumbered block identity")
		}
	}
}

func TestStreamBoundaryRejectsInconsistentBlockPayloads(t *testing.T) {
	for _, block := range []*schema.ContentBlock{
		{Type: schema.ContentBlockTypeAssistantGenText},
		{Type: schema.ContentBlockTypeAssistantGenText, AssistantGenText: &schema.AssistantGenText{Text: "text"}, FunctionToolCall: &schema.FunctionToolCall{CallID: "hidden", Name: "write", Arguments: `{}`}},
		{Type: schema.ContentBlockTypeFunctionToolResult, FunctionToolResult: &schema.FunctionToolResult{CallID: "forged"}},
		{Type: "unknown"},
	} {
		msg := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{block}, Extra: map[string]any{"seasprak.finish": "stop"}}
		vm := NewValidatedModel(malformedModel{msg: msg}, &factSink{}, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{})
		if _, err := vm.Generate(t.Context(), nil); err == nil {
			t.Errorf("accepted inconsistent block type %s", block.Type)
		}
	}
}

func TestStreamBoundaryPublishesBeforeFinalWithoutPrivateMetadata(t *testing.T) {
	sink := &factSink{}
	partial := 0
	inner := &p2BoundaryStream{beforeEnd: func() {
		for _, fact := range sink.facts {
			if fact.Kind != "model_stream_snapshot" {
				continue
			}
			partial++
			if strings.Contains(string(fact.Payload), "synthetic-private") {
				t.Error("private model metadata leaked in temporary snapshot")
			}
			var payload map[string]any
			if err := json.Unmarshal(fact.Payload, &payload); err != nil {
				t.Error(err)
			}
			if payload["attemptId"] == "" || payload["messageId"] == "" || payload["streamId"] == "" {
				t.Error("temporary update identity missing")
			}
		}
		if partial == 0 {
			t.Error("no temporary snapshot before model completion")
		}
	}}
	vm := NewValidatedModel(inner, sink, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{})
	r, err := vm.Stream(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if inner.requests != 1 {
		t.Fatal("extra stream requests")
	}
}
