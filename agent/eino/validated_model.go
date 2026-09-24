package eino

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/agent"
	product "github.com/ww1489/seasprak/model"
)

// ErrControlledStop ends the current inner execution without treating it as a model failure.
var ErrControlledStop = errors.New("controlled stop")

// ValidatedModel returns a tool-bearing message only after the full response is accepted.
type ValidatedModel struct {
	inner model.AgenticModel
	sink  agent.ExecutionSink
	budg  *agent.BudgetLedger
	scope agent.ExecutionScope
}

func NewValidatedModel(inner model.AgenticModel, sink agent.ExecutionSink, budg *agent.BudgetLedger, scope agent.ExecutionScope) *ValidatedModel {
	return &ValidatedModel{inner: inner, sink: sink, budg: budg, scope: scope}
}

func (m *ValidatedModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	if err := contextErr(ctx); err != nil {
		return nil, m.fail(ctx, nil, "failed", err)
	}
	if err := m.beginTurn(ctx); err != nil {
		return nil, err
	}
	if m.budg != nil {
		if err := m.budg.OccupyModel(); err != nil {
			return nil, m.fail(ctx, nil, "failed", err)
		}
	}
	msg, err := m.inner.Generate(ctx, input, opts...)
	if err != nil {
		return nil, m.fail(ctx, msg, "failed", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, m.fail(ctx, msg, "failed", err)
	}
	return m.accept(ctx, msg)
}

func (m *ValidatedModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	if err := contextErr(ctx); err != nil {
		return nil, m.fail(ctx, nil, "failed", err)
	}
	if err := m.beginTurn(ctx); err != nil {
		return nil, err
	}
	if m.budg != nil {
		if err := m.budg.OccupyModel(); err != nil {
			return nil, m.fail(ctx, nil, "failed", err)
		}
	}
	reader, err := m.inner.Stream(ctx, input, opts...)
	if err != nil {
		return nil, m.fail(ctx, nil, "failed", err)
	}
	defer reader.Close()
	var chunks []*schema.AgenticMessage
	for {
		chunk, recvErr := reader.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			partial, _ := schema.ConcatAgenticMessages(chunks)
			return nil, m.fail(ctx, partial, "incomplete", recvErr)
		}
		chunks = append(chunks, chunk)
	}
	msg, err := schema.ConcatAgenticMessages(chunks)
	if err != nil {
		return nil, m.fail(ctx, nil, "incomplete", err)
	}
	accepted, err := m.accept(ctx, msg)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{accepted}), nil
}

func (m *ValidatedModel) beginTurn(ctx context.Context) error {
	if m.budg == nil || turnStarted(ctx) {
		return nil
	}
	return m.budg.BeginTurn()
}

func (m *ValidatedModel) accept(ctx context.Context, msg *schema.AgenticMessage) (*schema.AgenticMessage, error) {
	if err := contextErr(ctx); err != nil {
		return nil, m.fail(ctx, stripTools(msg), "incomplete", err)
	}
	if msg == nil {
		return nil, m.fail(ctx, nil, "failed", product.NewError(product.CodeInternal, "empty model response"))
	}
	for _, block := range msg.ContentBlocks {
		if block == nil || (block.Type == schema.ContentBlockTypeFunctionToolCall && block.FunctionToolCall == nil) {
			return nil, m.fail(ctx, stripTools(msg), "incomplete", product.NewError(product.CodeInvalidArgument, "malformed model content block"))
		}
	}
	calls := toolCalls(msg)
	if msg.Role != schema.AgenticRoleTypeAssistant || !explicitSuccess(finishReason(msg)) || badToolCalls(calls) {
		return nil, m.fail(ctx, stripTools(msg), "incomplete", product.NewError(product.CodeInvalidArgument, "incomplete model response"))
	}
	if err := m.record(ctx, msg, "complete"); err != nil {
		return nil, err
	}
	return msg, nil
}

func (m *ValidatedModel) fail(ctx context.Context, msg *schema.AgenticMessage, status string, cause error) error {
	if recErr := m.record(ctx, msg, status); recErr != nil {
		if cause == nil {
			return recErr
		}
		return errors.Join(cause, recErr)
	}
	return cause
}

func (m *ValidatedModel) record(ctx context.Context, msg *schema.AgenticMessage, status string) error {
	payload, err := json.Marshal(map[string]any{"status": status, "message": msg})
	if err != nil {
		return err
	}
	return m.sink.CommitFact(ctx, ScopeFromContext(ctx, m.scope), agent.Fact{Kind: "assistant", Payload: payload})
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return product.NewError(product.CodeInvalidArgument, "context is required")
	}
	return ctx.Err()
}

func finishReason(msg *schema.AgenticMessage) string {
	if msg == nil || msg.Extra == nil {
		return ""
	}
	reason, _ := msg.Extra["seasprak.finish"].(string)
	return reason
}

func explicitSuccess(reason string) bool {
	return reason == "stop" || reason == "tool_calls"
}

func toolCalls(msg *schema.AgenticMessage) []*schema.FunctionToolCall {
	if msg == nil {
		return nil
	}
	var calls []*schema.FunctionToolCall
	for _, block := range msg.ContentBlocks {
		if block.Type == schema.ContentBlockTypeFunctionToolCall && block.FunctionToolCall != nil {
			calls = append(calls, block.FunctionToolCall)
		}
	}
	return calls
}

func badToolCalls(calls []*schema.FunctionToolCall) bool {
	seen := map[string]bool{}
	for _, call := range calls {
		if call.CallID == "" || call.Name == "" || seen[call.CallID] || !json.Valid([]byte(call.Arguments)) {
			return true
		}
		seen[call.CallID] = true
	}
	return false
}

func stripTools(msg *schema.AgenticMessage) *schema.AgenticMessage {
	if msg == nil {
		return nil
	}
	copied := *msg
	copied.ContentBlocks = nil
	for _, block := range msg.ContentBlocks {
		if block != nil && block.Type != schema.ContentBlockTypeFunctionToolCall {
			copied.ContentBlocks = append(copied.ContentBlocks, block)
		}
	}
	return &copied
}
