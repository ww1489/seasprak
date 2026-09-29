package llm

import (
	"context"
	"io"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	geminischema "github.com/cloudwego/eino/schema/gemini"
	product "github.com/ww1489/seasprak/internal/errors"
)

// Inject an invalid adapter result that JSON wire fixtures cannot express:
// the schema tag claims a function call while its typed payload is absent.
type geminiMissingPayloadModel struct{ calls int }

func (m *geminiMissingPayloadModel) Generate(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
	m.calls++
	return &schema.AgenticMessage{
		Role:          schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{{Type: schema.ContentBlockTypeFunctionToolCall}},
		ResponseMeta:  &schema.AgenticResponseMeta{GeminiExtension: &geminischema.ResponseMetaExtension{FinishReason: "STOP"}},
	}, nil
}

func (m *geminiMissingPayloadModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

func TestGeminiResponseRejectsMissingFunctionPayload(t *testing.T) {
	for _, mode := range []string{"generate", "stream"} {
		t.Run(mode, func(t *testing.T) {
			inner := &geminiMissingPayloadModel{}
			m := &geminiGenerateContentModel{inner: inner}
			input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
			var err error
			if mode == "generate" {
				var msg *schema.AgenticMessage
				msg, err = m.Generate(t.Context(), input)
				if msg != nil {
					t.Error("incomplete response returned a successful message")
				}
			} else {
				var reader *schema.StreamReader[*schema.AgenticMessage]
				reader, err = m.Stream(t.Context(), input)
				if err == nil {
					defer reader.Close()
					for {
						var msg *schema.AgenticMessage
						msg, err = reader.Recv()
						if err != nil {
							break
						}
						if msg != nil && msg.Extra["seasprak.finish"] != nil {
							t.Error("incomplete stream produced successful terminal metadata")
						}
					}
				}
			}
			productErr, ok := product.AsError(err)
			if err == nil || err == io.EOF || !ok || productErr.Code != product.CodeInvalidArgument {
				t.Errorf("expected invalid_argument, got %v", err)
			}
			if inner.calls != 1 {
				t.Errorf("adapter calls=%d, want 1", inner.calls)
			}
		})
	}
}
