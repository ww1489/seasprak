package consumer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

// The public model port uses Eino message/option types. The file backend and
// session consumer live separately and require only sdk and the standard library.
type consumerReadModel struct {
	mu           sync.Mutex
	initial      string
	calls        int
	pages        []consumerReadPage
	problem      string
	afterFirst   func()
	failedResult bool
}

func (m *consumerReadModel) Generate(_ context.Context, input []*schema.AgenticMessage, _ ...einomodel.Option) (*schema.AgenticMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	args := m.initial
	if m.calls > 1 {
		found := false
		for _, message := range input {
			for _, block := range message.ContentBlocks {
				result := block.FunctionToolResult
				if result == nil || result.CallID != fmt.Sprintf("read-%d", m.calls-1) {
					continue
				}
				found = true
				var page consumerReadPage
				var text string
				for _, content := range result.Content {
					if content.Text != nil {
						text += content.Text.Text
					}
				}
				if json.Unmarshal([]byte(text), &page) != nil || page.Version == "" {
					m.failedResult = true
					args = ""
					break
				}
				m.pages = append(m.pages, page)
				args = string(page.NextRead)
				if args == "null" {
					args = ""
				}
				if len(m.pages) == 1 && m.afterFirst != nil {
					m.afterFirst()
				}
			}
		}
		if !found {
			m.problem = "model did not receive the paired read result"
			args = ""
		}
	}
	if args == "" || m.calls > 8 {
		return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "done"})}, Extra: map[string]any{"seasprak.finish": "stop"}}, nil
	}
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: fmt.Sprintf("read-%d", m.calls), Name: "read_file", Arguments: args})}, Extra: map[string]any{"seasprak.finish": "tool_calls"}}, nil
}

func (m *consumerReadModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	message, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{message}), nil
}

var _ sdk.Model = (*consumerReadModel)(nil)
