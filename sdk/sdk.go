// Package sdk is the only public Seasprak entry. Types are aliases of the
// internal implementations so callers can declare and construct them without
// importing internal packages.
package sdk

import (
	"context"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

const (
	ProfileDefault = sessions.ProfileDefault
	ProfileMemory  = sessions.ProfileMemory

	CodeInvalidArgument        = product.CodeInvalidArgument
	CodeUnauthenticated        = product.CodeUnauthenticated
	CodePermissionDenied       = product.CodePermissionDenied
	CodeNotFound               = product.CodeNotFound
	CodeStateConflict          = product.CodeStateConflict
	CodeIdempotencyConflict    = product.CodeIdempotencyConflict
	CodeUnsupportedCapability  = product.CodeUnsupportedCapability
	CodeBudgetExhausted        = product.CodeBudgetExhausted
	CodeStorageUnavailable     = product.CodeStorageUnavailable
	CodeIncompatibleVersion    = product.CodeIncompatibleVersion
	CodeIncompatibleResume     = product.CodeIncompatibleResume
	CodeReconciliationRequired = product.CodeReconciliationRequired
	CodeResyncRequired         = product.CodeResyncRequired
	CodeResourceUnavailable    = product.CodeResourceUnavailable
	CodeInternal               = product.CodeInternal

	DecisionAllow  = agent.DecisionAllow
	DecisionDeny   = agent.DecisionDeny
	DecisionCancel = agent.DecisionCancel
	DecisionAsk    = agent.DecisionAsk
)

type (
	SessionOptions     = sessions.Options
	AgentSession       = sessions.AgentSession
	Snapshot           = sessions.Snapshot
	Subscription       = sessions.Subscription
	TraceState         = state.TraceState
	InputState         = state.InputState
	AgentMessage       = agent.AgentMessage
	TurnRecord         = agent.TurnRecord
	ToolRecord         = agent.ToolRecord
	ToolObservation    = agent.ToolObservation
	InputCommand       = agent.InputCommand
	InputReceipt       = agent.InputReceipt
	TargetAgent        = agent.TargetAgent
	Event              = agent.Event
	EventScope         = agent.EventScope
	MessageKind        = agent.MessageKind
	MessageStatus      = agent.MessageStatus
	MessageScope       = agent.MessageScope
	SourceKind         = agent.SourceKind
	SourceRef          = agent.SourceRef
	Usage              = agent.Usage
	ExecutionScope     = agent.ExecutionScope
	Fact               = agent.Fact
	FrozenCall         = agent.FrozenCall
	Decision           = agent.Decision
	ExecutionSink      = agent.ExecutionSink
	ToolAuthorizer     = agent.ToolAuthorizer
	BoundaryController = agent.BoundaryController
	TurnPlan           = agent.TurnPlan
	TurnFact           = agent.TurnFact
	BudgetLedger       = agent.BudgetLedger
	Error              = product.Error
	Limits             = config.Limits
	Model              = llm.Model
	ModelConfig        = llm.ModelConfig
	EffectiveOptions   = llm.EffectiveOptions
	ToolDefinition     = tools.Definition
	Executor           = tools.Executor
	Outcome            = tools.Outcome
	AgentDeps          = einorun.Deps
	SessionStore       = store.Store
	Commit             = store.Commit
	ExpectedCommit     = store.ExpectedCommit
	CommitReceipt      = store.CommitReceipt
	Header             = store.Header
	StoredSession      = store.StoredSession
	CommitReader       = store.CommitReader
	Record             = store.Record
	BranchUpdate       = store.BranchUpdate
)

func CreateAgentSession(ctx context.Context, opts SessionOptions) (*AgentSession, error) {
	return sessions.CreateAgentSession(ctx, opts)
}

func OpenAgentSession(ctx context.Context, opts SessionOptions) (*AgentSession, error) {
	return sessions.OpenAgentSession(ctx, opts)
}

func NewError(code, message string) *Error { return product.NewError(code, message) }

func Errorf(code, format string, args ...any) *Error { return product.Errorf(code, format, args...) }

func AsError(err error) (*Error, bool) { return product.AsError(err) }

func DefaultLimits() Limits { return config.DefaultLimits() }

func NewBudget(limits Limits) *BudgetLedger { return agent.NewBudget(limits) }

func NewExecutor(gen string, defs []ToolDefinition, sink ExecutionSink, auth ToolAuthorizer, budg *BudgetLedger) (*Executor, error) {
	return tools.NewExecutor(gen, defs, sink, auth, budg)
}

func NewAgent(ctx context.Context, deps AgentDeps) (adk.TypedResumableAgent[*schema.AgenticMessage], error) {
	return einorun.NewAgent(ctx, deps)
}
