package testkit

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Step is one scripted model response.
type Step struct {
	Text      string
	ToolCalls []schema.FunctionToolCall
	Err       error
	Truncated bool
	Gate      chan struct{}
	// Finish overrides seasprak.finish. NoFinish omits it so EOF is not success evidence.
	Finish   string
	NoFinish bool
	Role     schema.AgenticRoleType
	// Repeat keeps this step for every later call.
	Repeat bool
}

// FakeModel implements model.AgenticModel with a script and a call counter.
type FakeModel struct {
	mu    sync.Mutex
	steps []Step
	calls int
}

func NewFake(steps ...Step) *FakeModel { return &FakeModel{steps: steps} }

func (m *FakeModel) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *FakeModel) Generate(ctx context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
	step, err := m.next(ctx)
	if err != nil {
		return nil, err
	}
	return message(step), step.Err
}

func (m *FakeModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil && msg == nil {
		return nil, err
	}
	items := []*schema.AgenticMessage{}
	if msg != nil {
		items = append(items, msg)
	}
	return schema.StreamReaderFromArray(items), err
}

func (m *FakeModel) next(ctx context.Context) (Step, error) {
	m.mu.Lock()
	m.calls++
	if len(m.steps) == 0 {
		m.mu.Unlock()
		return Step{Text: "ok"}, nil
	}
	step := m.steps[0]
	if !step.Repeat {
		m.steps = m.steps[1:]
	}
	m.mu.Unlock()
	if step.Gate != nil {
		select {
		case <-step.Gate:
		case <-ctx.Done():
			return Step{}, ctx.Err()
		}
	}
	return step, nil
}

func message(step Step) *schema.AgenticMessage {
	blocks := []*schema.ContentBlock{}
	if step.Text != "" {
		blocks = append(blocks, schema.NewContentBlock(&schema.AssistantGenText{Text: step.Text}))
	}
	for i := range step.ToolCalls {
		call := step.ToolCalls[i]
		blocks = append(blocks, schema.NewContentBlock(&call))
	}
	role := step.Role
	if role == "" {
		role = schema.AgenticRoleTypeAssistant
	}
	msg := &schema.AgenticMessage{Role: role, ContentBlocks: blocks, Extra: map[string]any{}}
	switch {
	case step.NoFinish:
	case step.Finish != "":
		msg.Extra["seasprak.finish"] = step.Finish
	case step.Truncated:
		msg.Extra["seasprak.finish"] = "length"
	case len(step.ToolCalls) > 0:
		msg.Extra["seasprak.finish"] = "tool_calls"
	default:
		msg.Extra["seasprak.finish"] = "stop"
	}
	return msg
}
