package eino

import (
	"context"
	"github.com/ww1489/seasprak/internal/config"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
)

type malformedModel struct {
	msg    *schema.AgenticMessage
	cancel context.CancelFunc
}

func (m malformedModel) Generate(context.Context, []*schema.AgenticMessage, ...einomodel.Option) (*schema.AgenticMessage, error) {
	return m.msg, nil
}
func (m malformedModel) Stream(context.Context, []*schema.AgenticMessage, ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	if m.cancel != nil {
		m.cancel()
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{m.msg}), nil
}
func TestStreamCancellationBeforeAcceptance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msg := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "late"})}, Extra: map[string]any{"seasprak.finish": "stop"}}
	vm := NewValidatedModel(malformedModel{msg: msg, cancel: cancel}, &factSink{}, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{})
	reader, err := vm.Stream(ctx, nil)
	if reader != nil {
		reader.Close()
	}
	if err == nil {
		t.Fatal("cancelled stream accepted a complete response")
	}
}
func TestMalformedContentBlocksFailClosed(t *testing.T) {
	for _, blocks := range [][]*schema.ContentBlock{{nil}, {{Type: schema.ContentBlockTypeFunctionToolCall}}} {
		t.Run("malformed", func(t *testing.T) {
			msg := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: blocks, Extra: map[string]any{"seasprak.finish": "stop"}}
			vm := NewValidatedModel(malformedModel{msg: msg}, &factSink{}, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{})
			if _, err := vm.Generate(context.Background(), nil); err == nil {
				t.Fatal("malformed content accepted")
			}
		})
	}
}
