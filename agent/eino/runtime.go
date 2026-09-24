package eino

import (
	"context"
	"errors"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/prebuilt/deep"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/agent"
	product "github.com/ww1489/seasprak/model"
)

type Deps struct {
	Model       model.AgenticModel
	Tools       []tool.BaseTool
	Sink        agent.ExecutionSink
	Budget      *agent.BudgetLedger
	Boundary    agent.BoundaryController
	Instruction string
	Scope       agent.ExecutionScope
}

func NewAgent(ctx context.Context, deps Deps) (adk.TypedResumableAgent[*schema.AgenticMessage], error) {
	validated := NewValidatedModel(deps.Model, deps.Sink, deps.Budget, deps.Scope)
	maxIter := product.DefaultLimits().TraceLogicalModelCalls
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
			Tools: deps.Tools,
		}},
		Handlers: []adk.TypedChatModelAgentMiddleware[*schema.AgenticMessage]{
			&boundaryHandler{boundary: deps.Boundary, budget: deps.Budget, scope: deps.Scope},
		},
		ModelRetryConfig: &adk.TypedModelRetryConfig[*schema.AgenticMessage]{
			MaxRetries: 2,
			ShouldRetry: func(ctx context.Context, rc *adk.TypedRetryContext[*schema.AgenticMessage]) *adk.TypedRetryDecision[*schema.AgenticMessage] {
				return retryDecision(ctx, rc, deps.Budget)
			},
		},
	})
}

type boundaryHandler struct {
	adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]
	boundary agent.BoundaryController
	budget   *agent.BudgetLedger
	scope    agent.ExecutionScope
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
		if err := h.budget.BeginTurn(); err != nil {
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
		return ctx, state, nil
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

func retryDecision(ctx context.Context, rc *adk.TypedRetryContext[*schema.AgenticMessage], budg *agent.BudgetLedger) *adk.TypedRetryDecision[*schema.AgenticMessage] {
	no := &adk.TypedRetryDecision[*schema.AgenticMessage]{Retry: false}
	if ctx == nil || ctx.Err() != nil || rc == nil || rc.Err == nil {
		return no
	}
	var pe *product.Error
	if !errors.As(rc.Err, &pe) || pe == nil || !pe.Retryable {
		return no
	}
	if budg != nil && !budg.ModelRetryAllowed() {
		return no
	}
	return &adk.TypedRetryDecision[*schema.AgenticMessage]{Retry: true}
}
