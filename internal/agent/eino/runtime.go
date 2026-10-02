package eino

import (
	"context"
	"errors"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/llm"
	"sync"
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
	// ContextBudget is checked on the complete request before every model call.
	ContextBudget agent.ContextBudget
	// Compactor, when set, is the single product compaction service used for
	// the soft threshold and certified overflow recovery. Delegated children
	// leave it nil so they never write the parent history.
	Compactor agent.CompactionRequester
	// Name is the registered target name; empty keeps the historical "main".
	Name string
}

func NewAgent(ctx context.Context, deps Deps) (adk.TypedResumableAgent[*schema.AgenticMessage], error) {
	validated := NewValidatedModel(deps.Model, deps.Sink, deps.Budget, deps.Scope)
	maxIter := config.DefaultLimits().TraceLogicalModelCalls
	if deps.Budget != nil {
		maxIter = deps.Budget.Limits().TraceLogicalModelCalls
	}
	name := deps.Name
	if name == "" {
		name = agent.MainAgentName
	}
	contextBudget := deps.ContextBudget
	if deps.Compactor != nil && contextBudget.SoftRatio == 0 {
		contextBudget.SoftRatio = agent.DefaultSoftRatio
	}
	recoveries := config.DefaultLimits().OverflowRecoveries
	if deps.Budget != nil {
		recoveries = deps.Budget.Limits().OverflowRecoveries
	}
	overflow := &overflowRecovery{compactor: deps.Compactor, scope: deps.Scope, limit: recoveries, context: contextBudget}
	return deep.NewTyped(ctx, &deep.TypedConfig[*schema.AgenticMessage]{
		Name:                   name,
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
			&boundaryHandler{boundary: deps.Boundary, budget: deps.Budget, scope: deps.Scope, emptyInventory: len(deps.Tools) == 0, emptyCall: deps.EmptyInventoryCall, context: contextBudget, instruction: deps.Instruction, compactor: deps.Compactor},
		},
		ModelRetryConfig: &adk.TypedModelRetryConfig[*schema.AgenticMessage]{
			MaxRetries: 2,
			ShouldRetry: func(ctx context.Context, rc *adk.TypedRetryContext[*schema.AgenticMessage]) *adk.TypedRetryDecision[*schema.AgenticMessage] {
				if decision := overflow.decide(ctx, rc, deps.Budget); decision != nil {
					return decision
				}
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
	context        agent.ContextBudget
	instruction    string
	compactor      agent.CompactionRequester
}

func (h *boundaryHandler) BeforeModelRewriteState(ctx context.Context, state *adk.TypedChatModelAgentState[*schema.AgenticMessage], mc *adk.TypedModelContext[*schema.AgenticMessage]) (context.Context, *adk.TypedChatModelAgentState[*schema.AgenticMessage], error) {
	scope := h.scope
	if h.boundary != nil {
		plan, err := h.boundary.PrepareNextTurn(ctx, h.scope)
		if err != nil {
			return ctx, state, err
		}
		scope.TurnID = plan.TurnID
		scope.SelectionRevision = plan.SelectionRevision
		if plan.ToolsSelected {
			state.ToolInfos = append([]*schema.ToolInfo(nil), plan.ToolInfos...)
			state.DeferredToolInfos = nil
		}
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
		return ctx, state, h.checkContext(ctx, scope, state)
	}
	msg, err := h.boundary.TakeSteering(ctx, scope)
	if err != nil {
		return ctx, state, err
	}
	if msg == nil {
		return ctx, state, h.checkContext(ctx, scope, state)
	}
	projected, err := agent.ConvertToLLM([]agent.AgentMessage{*msg})
	if err != nil {
		return ctx, state, err
	}
	state.Messages = append(state.Messages, projected...)
	return ctx, state, h.checkContext(ctx, scope, state)
}

// checkContext requests one committed compaction when the complete request
// crosses the soft threshold, then rejects a request that still cannot fit
// the resolved window before any physical request is made.
func (h *boundaryHandler) checkContext(ctx context.Context, scope agent.ExecutionScope, state *adk.TypedChatModelAgentState[*schema.AgenticMessage]) error {
	if h.context.Window <= 0 && h.compactor == nil {
		return nil
	}
	var estimate agent.ContextEstimate
	if h.context.Window > 0 {
		var err error
		if estimate, err = agent.EstimateRequest(h.instruction, state.Messages, state.ToolInfos); err != nil {
			return err
		}
	}
	if h.compactor != nil {
		// Below the soft threshold the session only runs an intent that was
		// deferred during an approval wait; otherwise it returns immediately.
		reason := "boundary"
		if h.context.OverSoft(estimate) {
			reason = "soft_threshold"
		}
		messages, compacted, err := compactMessages(ctx, h.compactor, scope, state.Messages, reason)
		// A failed soft compaction keeps the old projection (08 §6); only
		// cancellation stops the request. The hard check below still applies.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(ctxErr, err)
		}
		if err == nil && compacted {
			state.Messages = messages
			if h.context.Window > 0 {
				if estimate, err = agent.EstimateRequest(h.instruction, state.Messages, state.ToolInfos); err != nil {
					return err
				}
			}
		}
	}
	if h.context.Window <= 0 {
		return nil
	}
	return h.context.Check(estimate)
}

// compactMessages asks the session for one committed compaction and rebuilds
// the model input as the leading instruction messages plus the new committed
// projection. The session refuses when the visible input is not exactly the
// committed projection, so no in-flight message is dropped.
func compactMessages(ctx context.Context, c agent.CompactionRequester, scope agent.ExecutionScope, current []*schema.AgenticMessage, reason string) ([]*schema.AgenticMessage, bool, error) {
	lead := 0
	for lead < len(current) && current[lead] != nil && current[lead].Role == schema.AgenticRoleTypeSystem {
		lead++
	}
	projected, compacted, err := c.CompactForRequest(ctx, scope, agent.CompactionRequest{Reason: reason, VisibleMessages: len(current) - lead})
	if err != nil || !compacted {
		return nil, false, err
	}
	converted, err := agent.ConvertToLLM(projected)
	if err != nil {
		return nil, false, err
	}
	out := make([]*schema.AgenticMessage, 0, lead+len(converted))
	out = append(out, current[:lead]...)
	return append(out, converted...), true, nil
}

// overflowRecovery allows OverflowRecoveries retries per logical generation
// after a certified context overflow, and only when the compaction service
// committed a replacement projection. Without one the original failure is kept.
type overflowRecovery struct {
	compactor agent.CompactionRequester
	scope     agent.ExecutionScope
	limit     int
	context   agent.ContextBudget
	mu        sync.Mutex
	callID    string
	used      int
}

func (r *overflowRecovery) decide(ctx context.Context, rc *adk.TypedRetryContext[*schema.AgenticMessage], budg *agent.BudgetLedger) *adk.TypedRetryDecision[*schema.AgenticMessage] {
	if r == nil || r.compactor == nil || ctx == nil || ctx.Err() != nil || rc == nil || rc.Err == nil || rc.OutputMessage != nil {
		return nil
	}
	if info, ok := llm.ModelFailure(rc.Err); !ok || info.Kind != "context_overflow" {
		return nil
	}
	no := &adk.TypedRetryDecision[*schema.AgenticMessage]{Retry: false}
	scope := ScopeFromContext(ctx, r.scope)
	callID := scope.TurnID
	if budg != nil {
		callID = budg.Snapshot().ModelCallID
	}
	r.mu.Lock()
	if r.callID != callID {
		r.callID, r.used = callID, 0
	}
	exhausted := r.used >= r.limit
	if !exhausted {
		r.used++
	}
	r.mu.Unlock()
	if exhausted || (budg != nil && !budg.ModelRetryAllowed()) {
		return no
	}
	messages, compacted, err := compactMessages(ctx, r.compactor, scope, rc.InputMessages, "overflow")
	if err != nil {
		return &adk.TypedRetryDecision[*schema.AgenticMessage]{Retry: false, RewriteError: errors.Join(rc.Err, err)}
	}
	if !compacted {
		return no
	}
	// The replacement must still be able to fit; the lower bound excludes tools.
	if r.context.Window > 0 {
		if estimate, err := agent.EstimateRequest("", messages, nil); err != nil || !r.context.Fits(estimate) {
			return no
		}
	}
	return &adk.TypedRetryDecision[*schema.AgenticMessage]{Retry: true, ModifiedInputMessages: messages, PersistModifiedInputMessages: true, Backoff: time.Millisecond}
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
