package eino

import (
	"context"
	"errors"
	"github.com/ww1489/seasprak/internal/config"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/prebuilt/deep"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

type Deps struct {
	Model               model.AgenticModel
	Tools               []tool.BaseTool
	Sink                agent.ExecutionSink
	Budget              *agent.BudgetLedger
	Boundary            agent.BoundaryController
	Instruction         string
	Scope               agent.ExecutionScope
	RemainingActivity   func() time.Duration
	UnknownToolsHandler func(context.Context, string, string) (string, error)
	EmptyInventoryCall  func(context.Context, string, string, string) error
}

func NewAgent(ctx context.Context, deps Deps) (adk.TypedResumableAgent[*schema.AgenticMessage], error) {
	validated := NewValidatedModel(deps.Model, deps.Sink, deps.Budget, deps.Scope)
	maxIter := config.DefaultLimits().TraceLogicalModelCalls
	if deps.Budget != nil {
		maxIter = deps.Budget.Limits().TraceLogicalModelCalls
	}
	return deep.NewTyped(ctx, &deep.TypedConfig[*schema.AgenticMessage]{
		Name:                   "main",
		ChatModel:              validated,
		Instruction:            deps.Instruction,
		WithoutWriteTodos:      true,
		WithoutGeneralSubAgent: true,
		MaxIteration:           maxIter,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools:               deps.Tools,
			UnknownToolsHandler: deps.UnknownToolsHandler,
		}},
		Handlers: []adk.TypedChatModelAgentMiddleware[*schema.AgenticMessage]{
			&boundaryHandler{boundary: deps.Boundary, budget: deps.Budget, scope: deps.Scope, emptyInventory: len(deps.Tools) == 0, emptyCall: deps.EmptyInventoryCall},
		},
		ModelRetryConfig: &adk.TypedModelRetryConfig[*schema.AgenticMessage]{
			MaxRetries: 2,
			ShouldRetry: func(ctx context.Context, rc *adk.TypedRetryContext[*schema.AgenticMessage]) *adk.TypedRetryDecision[*schema.AgenticMessage] {
				return retryDecisionWithTiming(ctx, rc, deps.Budget, retryTiming{remaining: deps.RemainingActivity})
			},
		},
	})
}

type boundaryHandler struct {
	adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]
	boundary       agent.BoundaryController
	budget         *agent.BudgetLedger
	scope          agent.ExecutionScope
	emptyInventory bool
	emptyCall      func(context.Context, string, string, string) error
}

func (h *boundaryHandler) BeforeModelRewriteState(ctx context.Context, state *adk.TypedChatModelAgentState[*schema.AgenticMessage], mc *adk.TypedModelContext[*schema.AgenticMessage]) (context.Context, *adk.TypedChatModelAgentState[*schema.AgenticMessage], error) {
	scope := h.scope
	if h.boundary != nil {
		plan, err := h.boundary.PrepareNextTurn(ctx, h.scope)
		if err != nil {
			return ctx, state, err
		}
		scope.TurnID = plan.TurnID
	}
	if h.budget != nil {
		var err error
		if scope.TurnID != "" {
			err = h.budget.BeginTurnID(scope.TurnID)
		} else {
			err = h.budget.BeginTurn()
		}
		if err != nil {
			return ctx, state, err
		}
	}
	ctx = markTurn(noteScope(ctx, scope))
	if h.boundary == nil {
		return ctx, state, nil
	}
	msg, err := h.boundary.TakeSteering(ctx, scope)
	if err != nil || msg == nil {
		return ctx, state, err
	}
	projected, err := agent.ConvertToLLM([]agent.AgentMessage{*msg})
	if err != nil {
		return ctx, state, err
	}
	state.Messages = append(state.Messages, projected...)
	return ctx, state, nil
}

func (h *boundaryHandler) AfterModelRewriteState(ctx context.Context, state *adk.TypedChatModelAgentState[*schema.AgenticMessage], mc *adk.TypedModelContext[*schema.AgenticMessage]) (context.Context, *adk.TypedChatModelAgentState[*schema.AgenticMessage], error) {
	if h.boundary == nil || len(state.Messages) == 0 {
		return ctx, state, nil
	}
	scope := ScopeFromContext(ctx, h.scope)
	last := state.Messages[len(state.Messages)-1]
	if hasToolCall(last) {
		if !h.emptyInventory || h.emptyCall == nil {
			return ctx, state, nil
		}
		for _, block := range last.ContentBlocks {
			if block != nil && block.FunctionToolCall != nil {
				call := block.FunctionToolCall
				if err := h.emptyCall(ctx, call.CallID, call.Name, call.Arguments); err != nil {
					return ctx, state, err
				}
			}
		}
	}
	stop, reason, err := h.boundary.ShouldStop(ctx, scope)
	if err != nil {
		return ctx, state, err
	}
	if err := h.boundary.FinishTurn(ctx, scope, agent.TurnFact{TurnID: scope.TurnID, Stop: stop, StopReason: reason}); err != nil {
		return ctx, state, err
	}
	return ctx, state, nil
}

func hasToolCall(msg *schema.AgenticMessage) bool {
	if msg == nil {
		return false
	}
	for _, block := range msg.ContentBlocks {
		if block.Type == schema.ContentBlockTypeFunctionToolCall {
			return true
		}
	}
	return false
}

func FinishAfterTools(boundary agent.BoundaryController, scope agent.ExecutionScope) func(context.Context) error {
	return func(ctx context.Context) error {
		if boundary == nil {
			return nil
		}
		current := ScopeFromContext(ctx, scope)
		stop, reason, err := boundary.ShouldStop(ctx, current)
		if err != nil {
			return err
		}
		if err := boundary.FinishTurn(ctx, current, agent.TurnFact{TurnID: current.TurnID, HasTools: true, Stop: stop, StopReason: reason}); err != nil {
			return err
		}
		if stop {
			return ErrControlledStop
		}
		return nil
	}
}

// A joined persistence/cancellation failure must dominate a transient provider
// error. errors.As alone would select only the first matching product error.
func retryableModelError(err error) bool {
	var persistence *attemptPersistenceError
	if errors.As(err, &persistence) {
		return false
	}
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if pe, ok := err.(*product.Error); ok {
		return pe != nil && pe.Code == product.CodeResourceUnavailable && pe.Retryable
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		items := joined.Unwrap()
		if len(items) == 0 {
			return false
		}
		for _, item := range items {
			if !retryableModelError(item) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return retryableModelError(wrapped.Unwrap())
	}
	return false
}

func retryDecision(ctx context.Context, rc *adk.TypedRetryContext[*schema.AgenticMessage], budg *agent.BudgetLedger) *adk.TypedRetryDecision[*schema.AgenticMessage] {
	return retryDecisionWithTiming(ctx, rc, budg, retryTiming{})
}
func retryDecisionWithTiming(ctx context.Context, rc *adk.TypedRetryContext[*schema.AgenticMessage], budg *agent.BudgetLedger, timing retryTiming) *adk.TypedRetryDecision[*schema.AgenticMessage] {
	no := &adk.TypedRetryDecision[*schema.AgenticMessage]{Retry: false}
	if ctx == nil || ctx.Err() != nil || rc == nil || rc.Err == nil || rc.OutputMessage != nil {
		return no
	}
	if !retryableModelError(rc.Err) {
		return no
	}
	if budg != nil && !budg.ModelRetryAllowed() {
		return no
	}
	delay, allowed := timing.delay(ctx, rc.RetryAttempt, rc.Err)
	if !allowed {
		return no
	}
	return &adk.TypedRetryDecision[*schema.AgenticMessage]{Retry: true, Backoff: delay}
}
