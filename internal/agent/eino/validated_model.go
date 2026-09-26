package eino

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
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
	var requestErr error
	ctx, requestErr = m.requestContext(ctx)
	if requestErr != nil {
		if _, started := attemptFromContext(ctx); !started {
			return nil, requestErr
		}
		return nil, m.fail(ctx, nil, "failed", requestErr)
	}
	if err := contextErr(ctx); err != nil {
		return nil, m.fail(ctx, nil, "failed", err)
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
	var requestErr error
	ctx, requestErr = m.requestContext(ctx)
	if requestErr != nil {
		if _, started := attemptFromContext(ctx); !started {
			return nil, requestErr
		}
		return nil, m.fail(ctx, nil, "failed", requestErr)
	}
	if err := contextErr(ctx); err != nil {
		return nil, m.fail(ctx, nil, "failed", err)
	}
	reader, err := m.inner.Stream(ctx, input, opts...)
	if reader != nil {
		defer reader.Close()
	}
	if err != nil {
		return nil, m.fail(ctx, nil, "failed", err)
	}
	if reader == nil {
		return nil, m.fail(ctx, nil, "failed", product.NewError(product.CodeInternal, "model stream is missing"))
	}
	var chunks []*schema.AgenticMessage
	var chunkSeq uint64
	blockIndices := map[int]bool{}
	for {
		if err := ctx.Err(); err != nil {
			partial, _ := schema.ConcatAgenticMessages(chunks)
			return nil, m.fail(ctx, partial, "aborted", err)
		}
		chunk, recvErr := reader.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			partial, _ := schema.ConcatAgenticMessages(chunks)
			return nil, m.fail(ctx, partial, "incomplete", recvErr)
		}
		chunks = append(chunks, chunk)
		partial, concatErr := schema.ConcatAgenticMessages(chunks)
		if concatErr != nil {
			prior, _ := schema.ConcatAgenticMessages(chunks[:len(chunks)-1])
			return nil, m.fail(ctx, prior, "incomplete", product.NewError(product.CodeInvalidArgument, "model stream aggregation failed"))
		}
		chunkSeq++
		var indices []int
		if chunk != nil {
			for _, block := range chunk.ContentBlocks {
				if block != nil && block.StreamingMeta != nil {
					blockIndices[block.StreamingMeta.Index] = true
				}
			}
		}
		for index := range blockIndices {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		if err := m.observe(ctx, partial, chunkSeq, indices); err != nil {
			return nil, m.fail(ctx, partial, "incomplete", err)
		}
	}
	msg, err := schema.ConcatAgenticMessages(chunks)
	if err != nil {
		return nil, m.fail(ctx, nil, "incomplete", product.NewError(product.CodeInvalidArgument, "model stream aggregation failed"))
	}
	accepted, err := m.accept(ctx, msg)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{accepted}), nil
}

// requestContext switches accounting only for explicitly transport-observed
// models. The legacy injection path keeps its P1 call-level budget contract.
type attemptPersistenceError struct{ cause error }

func (e *attemptPersistenceError) Error() string { return e.cause.Error() }
func (e *attemptPersistenceError) Unwrap() error { return e.cause }

type attemptContextKey struct{}

func attemptFromContext(ctx context.Context) (agent.ModelAttemptIdentity, bool) {
	if ctx == nil {
		return agent.ModelAttemptIdentity{}, false
	}
	identity, ok := ctx.Value(attemptContextKey{}).(agent.ModelAttemptIdentity)
	return identity, ok
}

func (m *ValidatedModel) requestContext(ctx context.Context) (context.Context, error) {
	identity := agent.ModelAttemptIdentity{ID: agent.MustID(), ModelCallID: ScopeFromContext(ctx, m.scope).TurnID, MessageID: agent.MustID(), StreamID: agent.MustID()}
	if m.budg != nil {
		identity.ModelCallID = m.budg.Snapshot().ModelCallID
	}
	if identity.ModelCallID == "" {
		identity.ModelCallID = agent.MustID()
	}
	if configured, ok := m.inner.(interface{ Configuration() llm.ModelConfig }); ok {
		identity.ModelConfigVersion = configured.Configuration().Version
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		return ctx, err
	}
	if err := m.sink.CommitFact(ctx, ScopeFromContext(ctx, m.scope), agent.Fact{Kind: "model_attempt_started", Payload: payload}); err != nil {
		return ctx, &attemptPersistenceError{cause: err}
	}
	ctx = context.WithValue(ctx, attemptContextKey{}, identity)
	usage := &attemptUsage{identity: identity, items: map[uint64]agent.ModelRequestUsage{}}
	ctx = llm.WithUsageObservation(context.WithValue(ctx, attemptUsageKey{}, usage), usage)
	if !llm.UsesObservedTransport(m.inner) {
		if m.budg != nil {
			return ctx, m.budg.OccupyModel()
		}
		return ctx, nil
	}
	if m.budg == nil {
		return ctx, product.NewError(product.CodeInvalidArgument, "observed model requires a budget")
	}
	if !m.budg.ModelRetryAllowed() {
		return ctx, product.NewError(product.CodeBudgetExhausted, "model budget exhausted")
	}
	request := llm.RequestIdentity{ModelCallID: identity.ModelCallID, AttemptID: identity.ID, Purpose: "agent"}
	return llm.WithRequestObservation(ctx, request, m.budg), nil
}

func (m *ValidatedModel) beginTurn(ctx context.Context) error {
	if m.budg == nil || turnStarted(ctx) {
		return nil
	}
	if scope := ScopeFromContext(ctx, m.scope); scope.TurnID != "" {
		return m.budg.BeginTurnID(scope.TurnID)
	}
	return m.budg.BeginTurn()
}

func (m *ValidatedModel) observe(ctx context.Context, msg *schema.AgenticMessage, seq uint64, indices []int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	identity, ok := attemptFromContext(ctx)
	if !ok || msg == nil {
		return nil
	}
	update := agent.ModelStreamSnapshot{AttemptID: identity.ID, MessageID: identity.MessageID, StreamID: identity.StreamID, ChunkSeq: seq}
	for index, block := range msg.ContentBlocks {
		if block == nil {
			continue
		}
		visible := agent.ModelStreamBlock{BlockIndex: index, Type: string(block.Type)}
		if index < len(indices) {
			visible.BlockIndex = indices[index]
		}
		switch {
		case block.AssistantGenText != nil:
			visible.Text = block.AssistantGenText.Text
		case block.Reasoning != nil:
			visible.Text = block.Reasoning.Text
		case block.FunctionToolCall != nil:
			visible.CallID, visible.Name, visible.Arguments = block.FunctionToolCall.CallID, block.FunctionToolCall.Name, block.FunctionToolCall.Arguments
		default:
			continue
		}
		update.Blocks = append(update.Blocks, visible)
	}
	payload, err := json.Marshal(update)
	if err != nil {
		return err
	}
	return m.sink.CommitFact(ctx, ScopeFromContext(ctx, m.scope), agent.Fact{Kind: "model_stream_snapshot", Payload: payload})
}

func (m *ValidatedModel) accept(ctx context.Context, msg *schema.AgenticMessage) (*schema.AgenticMessage, error) {
	if err := contextErr(ctx); err != nil {
		return nil, m.fail(ctx, stripTools(msg), "incomplete", err)
	}
	if msg == nil {
		return nil, m.fail(ctx, nil, "failed", product.NewError(product.CodeInternal, "empty model response"))
	}
	for _, block := range msg.ContentBlocks {
		if !validAssistantBlock(block) {
			return nil, m.fail(ctx, stripTools(msg), "incomplete", product.NewError(product.CodeInvalidArgument, "malformed model content block"))
		}
	}
	calls := toolCalls(msg)
	if msg.Role != schema.AgenticRoleTypeAssistant || !explicitSuccess(finishReason(msg)) || badToolCalls(calls) {
		return nil, m.fail(ctx, stripTools(msg), "incomplete", product.NewError(product.CodeInvalidArgument, "incomplete model response"))
	}
	if err := m.record(ctx, msg, "complete", nil); err != nil {
		return nil, m.fail(ctx, stripTools(msg), "incomplete", err)
	}
	return msg, nil
}

func (m *ValidatedModel) fail(ctx context.Context, msg *schema.AgenticMessage, status string, cause error) error {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		status = "aborted"
	}
	if recErr := m.record(ctx, msg, status, cause); recErr != nil {
		if cause == nil {
			return recErr
		}
		return errors.Join(cause, recErr)
	}
	return cause
}

func (m *ValidatedModel) record(ctx context.Context, msg *schema.AgenticMessage, status string, cause error) error {
	identity, _ := attemptFromContext(ctx)
	details := attemptDetails(ctx, cause)
	if status != "complete" {
		details.RefusalReason, details.OriginalFinishReason = llm.ResponseDiagnostic(msg)
		if finishReason(msg) == "refusal" && details.FailureCode == product.CodeInvalidArgument {
			details.FailureReason = "refusal"
		}
		msg = diagnosticPartial(msg)
	}
	payload, err := json.Marshal(map[string]any{"status": status, "message": msg, "attemptId": identity.ID, "details": details})
	if err != nil {
		return err
	}
	if err := m.sink.CommitFact(ctx, ScopeFromContext(ctx, m.scope), agent.Fact{Kind: "assistant", Payload: payload}); err != nil {
		return &attemptPersistenceError{cause: err}
	}
	return nil
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

func validAssistantBlock(block *schema.ContentBlock) bool {
	if block == nil {
		return false
	}
	populated := 0
	for _, present := range []bool{
		block.Reasoning != nil, block.AssistantGenText != nil, block.AssistantGenImage != nil, block.AssistantGenAudio != nil, block.AssistantGenVideo != nil, block.FunctionToolCall != nil,
		block.UserInputText != nil, block.UserInputImage != nil, block.UserInputAudio != nil, block.UserInputVideo != nil, block.UserInputFile != nil, block.FunctionToolResult != nil,
		block.ToolSearchFunctionToolResult != nil, block.ServerToolCall != nil, block.ServerToolResult != nil, block.MCPToolCall != nil, block.MCPToolResult != nil, block.MCPListToolsResult != nil, block.MCPToolApprovalRequest != nil, block.MCPToolApprovalResponse != nil,
	} {
		if present {
			populated++
		}
	}
	if populated != 1 {
		return false
	}
	switch block.Type {
	case schema.ContentBlockTypeReasoning:
		return block.Reasoning != nil
	case schema.ContentBlockTypeAssistantGenText:
		return block.AssistantGenText != nil
	case schema.ContentBlockTypeAssistantGenImage:
		return block.AssistantGenImage != nil
	case schema.ContentBlockTypeAssistantGenAudio:
		return block.AssistantGenAudio != nil
	case schema.ContentBlockTypeAssistantGenVideo:
		return block.AssistantGenVideo != nil
	case schema.ContentBlockTypeFunctionToolCall:
		return block.FunctionToolCall != nil
	default:
		return false
	}
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
