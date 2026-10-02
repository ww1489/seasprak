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
	inner   model.AgenticModel
	sink    agent.ExecutionSink
	budg    *agent.BudgetLedger
	scope   agent.ExecutionScope
	purpose string
	callID  string // fixed for a workflow node; summaries start a fresh call
}

type resolvedModelKey struct{}

func (m *ValidatedModel) resolveModel(ctx context.Context) (model.AgenticModel, error) {
	if ctx != nil {
		if resolved, ok := ctx.Value(resolvedModelKey{}).(model.AgenticModel); ok && resolved != nil {
			return resolved, nil
		}
	}
	if resolver, ok := m.inner.(interface {
		ResolveModel(context.Context, agent.ExecutionScope) (model.AgenticModel, error)
	}); ok {
		return resolver.ResolveModel(ctx, ScopeFromContext(ctx, m.scope))
	}
	return m.inner, nil
}

func NewValidatedModel(inner model.AgenticModel, sink agent.ExecutionSink, budg *agent.BudgetLedger, scope agent.ExecutionScope) *ValidatedModel {
	return &ValidatedModel{inner: inner, sink: sink, budg: budg, scope: scope, purpose: "agent"}
}

// NewAuxiliaryModel uses the same attempt and transport validation without
// inheriting an enclosing agent's active logical call. Auxiliary responses
// cannot admit tools; workflow calls retain their original node identity.
func NewAuxiliaryModel(inner model.AgenticModel, sink agent.ExecutionSink, budg *agent.BudgetLedger, scope agent.ExecutionScope, purpose, callID string) (*ValidatedModel, error) {
	if purpose != "compaction" && purpose != "workflow_node" || purpose == "workflow_node" && callID == "" || purpose == "compaction" && callID != "" {
		return nil, product.NewError(product.CodeInvalidArgument, "auxiliary model purpose and logical identity are invalid")
	}
	return &ValidatedModel{inner: inner, sink: sink, budg: budg, scope: scope, purpose: purpose, callID: callID}, nil
}

func (m *ValidatedModel) requestBase(ctx context.Context) context.Context {
	if ctx == nil || m.purpose == "agent" {
		return ctx
	}
	ctx = WithExecutionScope(ctx, m.scope)
	ctx = context.WithValue(ctx, resolvedModelKey{}, struct{}{})
	return context.WithValue(ctx, attemptContextKey{}, struct{}{})
}

func (m *ValidatedModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	ctx = m.requestBase(ctx)
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
	inner, err := m.resolveModel(ctx)
	if err != nil {
		return nil, m.fail(ctx, nil, "failed", err)
	}
	msg, err := inner.Generate(ctx, input, opts...)
	if err != nil {
		return nil, m.fail(ctx, msg, "failed", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, m.fail(ctx, msg, "failed", err)
	}
	return m.accept(ctx, msg)
}

func (m *ValidatedModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	ctx = m.requestBase(ctx)
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
	inner, err := m.resolveModel(ctx)
	if err != nil {
		return nil, m.fail(ctx, nil, "failed", err)
	}
	reader, err := inner.Stream(ctx, input, opts...)
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
	inner, err := m.resolveModel(ctx)
	if err != nil {
		return ctx, err
	}
	ctx = context.WithValue(ctx, resolvedModelKey{}, inner)
	identity := agent.ModelAttemptIdentity{ID: agent.MustID(), ModelCallID: ScopeFromContext(ctx, m.scope).TurnID, MessageID: agent.MustID(), StreamID: agent.MustID(), Purpose: m.purpose}
	if m.budg != nil {
		identity.ModelCallID = m.budg.Snapshot().ModelCallID
	}
	if identity.ModelCallID == "" {
		identity.ModelCallID = agent.MustID()
	}
	if configured, ok := inner.(interface{ Configuration() llm.ModelConfig }); ok {
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
	if !llm.UsesObservedTransport(inner) {
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
	request := llm.RequestIdentity{ModelCallID: identity.ModelCallID, AttemptID: identity.ID, Purpose: m.purpose}
	scope := ScopeFromContext(ctx, m.scope)
	cacheScope := ""
	if scope.SessionID != "" {
		cacheScope = "code:" + scope.SessionID
	}
	if scope.WorkflowRunID != "" {
		if scope.SessionID != "" || scope.TraceID != "" || scope.TurnID != "" {
			return ctx, product.NewError(product.CodeInvalidArgument, "model cache scope has conflicting execution roots")
		}
		cacheScope = "workflow:" + scope.WorkflowRunID
	}
	ctx = llm.WithSessionCacheScope(ctx, cacheScope)
	return llm.WithRequestObservation(ctx, request, m.budg), nil
}

func (m *ValidatedModel) beginTurn(ctx context.Context) error {
	if m.budg == nil {
		return nil
	}
	if m.purpose != "agent" {
		if m.callID != "" {
			return m.budg.BeginTurnID(m.callID)
		}
		return m.budg.BeginTurn()
	}
	if turnStarted(ctx) {
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
	if err := agent.ValidateModelResponse(msg, m.purpose == "agent"); err != nil {
		return nil, m.fail(ctx, stripTools(msg), "incomplete", err)
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
