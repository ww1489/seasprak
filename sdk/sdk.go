// Package sdk is the only public Seasprak entry. Types are aliases of the
// internal implementations so callers can declare and construct them without
// importing internal packages.
package sdk

import (
	"context"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/workflowagent"
)

const (
	ProfileDefault = codeagent.ProfileDefault
	ProfileMemory  = codeagent.ProfileMemory

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

	KindUser              = agent.KindUser
	KindAssistant         = agent.KindAssistant
	KindToolResult        = agent.KindToolResult
	KindCustom            = agent.KindCustom
	KindCommand           = agent.KindCommand
	KindCompactionSummary = agent.KindCompactionSummary
	KindBranchSummary     = agent.KindBranchSummary
	KindOpaque            = agent.KindOpaque

	StatusComplete   = agent.StatusComplete
	StatusIncomplete = agent.StatusIncomplete

	SourceHuman        = agent.SourceHuman
	SourceDirectParent = agent.SourceDirectParent
	SourceExtension    = agent.SourceExtension
	SourceTool         = agent.SourceTool
	SourceModel        = agent.SourceModel
	SourceResource     = agent.SourceResource
	SourceImported     = agent.SourceImported
)

// ErrControlledStop ends an execution at a boundary without a model failure.
// Use errors.Is to recognize it in Eino runner events.
var ErrControlledStop = einorun.ErrControlledStop

type (
	WorkflowAgent               = workflowagent.WorkflowAgent
	WorkflowOptions             = workflowagent.WorkflowOptions
	WorkflowInputCommand        = workflowagent.WorkflowInputCommand
	WorkflowInputReceipt        = workflowagent.WorkflowInputReceipt
	WorkflowControlCommand      = workflowagent.WorkflowControlCommand
	WorkflowInteractionResponse = workflowagent.WorkflowInteractionResponse
	WorkflowOperationReceipt    = workflowagent.WorkflowOperationReceipt
	WorkflowOperation           = workflowagent.WorkflowOperation
	WorkflowSnapshot            = workflowagent.WorkflowSnapshot
	WorkflowModelAttemptView    = workflowagent.WorkflowModelAttemptView
	WorkflowObservationView     = workflowagent.WorkflowObservationView
	WorkflowSubscribeOptions    = workflowagent.WorkflowSubscribeOptions
	WorkflowSubscription        = workflowagent.WorkflowSubscription
	WorkflowInteraction         = workflowagent.WorkflowInteraction
	WorkflowRunNode             = workflowagent.NodeRun
	WorkflowTransient           = workflowagent.WorkflowTransient
	WorkflowToolPreview         = workflowagent.WorkflowToolPreview
	ResolvedPolicy              = agent.ResolvedPolicy
	SessionOptions              = codeagent.Options
	AgentSession                = codeagent.AgentSession
	Snapshot                    = codeagent.Snapshot
	ModelAttemptView            = codeagent.ModelAttemptView
	ObservationView             = codeagent.ObservationView
	ResumeCommand               = codeagent.ResumeCommand
	InteractionResponse         = codeagent.InteractionResponse
	Interaction                 = state.Interaction
	Approval                    = state.Approval
	Reconciliation              = state.Reconciliation
	Invocation                  = state.Invocation
	ReconcileCommand            = codeagent.ReconcileCommand
	ReconcileEvidence           = codeagent.ReconcileEvidence
	ReconcileQuery              = codeagent.ReconcileQuery
	ReconcileQueryRequest       = codeagent.ReconcileQueryRequest
	ReconcileQueryFunc          = codeagent.ReconcileQueryFunc
	CommandRequest              = codeagent.CommandRequest
	CommandResult               = codeagent.CommandResult
	FrozenExecution             = agent.FrozenExecution
	ResumeEligibility           = codeagent.ResumeEligibility
	PendingReconciliation       = codeagent.PendingReconciliation
	OperationReceipt            = state.OperationReceipt
	OperationStatus             = state.OperationStatus
	Selection                   = state.Selection
	ModelChoice                 = codeagent.ModelChoice
	SetDefaultModelRequest      = codeagent.SetDefaultModelRequest
	SelectNextTurnModelRequest  = codeagent.SelectNextTurnModelRequest
	SetActiveToolsRequest       = codeagent.SetActiveToolsRequest
	SearchToolsRequest          = codeagent.SearchToolsRequest
	CancelTraceRequest          = codeagent.CancelTraceRequest
	ContinueQueueRequest        = codeagent.ContinueQueueRequest
	Capabilities                = codeagent.Capabilities
	AgentDefinition             = agent.AgentDefinition
	AgentInfo                   = agent.AgentInfo
	WorkflowDefinition          = workflowagent.WorkflowDefinition
	WorkflowNode                = workflowagent.WorkflowNode
	WorkflowValue               = workflowagent.WorkflowValue
	WorkflowRef                 = workflowagent.WorkflowRef
	WorkflowCondition           = workflowagent.WorkflowCondition
	WorkflowEdge                = workflowagent.WorkflowEdge
	CompiledWorkflow            = workflowagent.CompiledWorkflow
	ToolCandidate               = codeagent.ToolCandidate
	Subscription                = codeagent.Subscription
	TraceState                  = state.TraceState
	InputState                  = state.InputState
	AgentMessage                = agent.AgentMessage
	CustomMessage               = agent.CustomMessage
	SummaryMessage              = agent.SummaryMessage
	CommandMessage              = agent.CommandMessage
	OpaqueMessage               = agent.OpaqueMessage
	TurnRecord                  = agent.TurnRecord
	ToolRecord                  = agent.ToolRecord
	ToolObservation             = agent.ToolObservation
	ToolOutputChunk             = agent.ToolOutputChunk
	ToolOutputSink              = agent.ToolOutputSink
	ToolOutputDelta             = agent.ToolOutputDelta
	InputCommand                = agent.InputCommand
	InputReceipt                = agent.InputReceipt
	TargetAgent                 = agent.TargetAgent
	Event                       = agent.Event
	EventScope                  = agent.EventScope
	MessageKind                 = agent.MessageKind
	MessageStatus               = agent.MessageStatus
	MessageScope                = agent.MessageScope
	SourceKind                  = agent.SourceKind
	SourceRef                   = agent.SourceRef
	Usage                       = agent.Usage
	ExecutionScope              = agent.ExecutionScope
	Fact                        = agent.Fact
	FrozenCall                  = agent.FrozenCall
	Decision                    = agent.Decision
	ExecutionSink               = agent.ExecutionSink
	ToolAuthorizer              = agent.ToolAuthorizer
	BoundaryController          = agent.BoundaryController
	TurnPlan                    = agent.TurnPlan
	TurnFact                    = agent.TurnFact
	BudgetLedger                = agent.BudgetLedger
	Error                       = product.Error
	Limits                      = config.Limits
	Model                       = llm.Model
	ModelConfig                 = llm.ModelConfig
	ModelKey                    = llm.ModelKey
	RequestedOptions            = llm.RequestedOptions
	EffectiveOptions            = llm.EffectiveOptions
	Catalog                     = llm.Catalog
	Capability                  = llm.Capability
	CapabilityName              = llm.CapabilityName
	ModelCapabilities           = llm.ModelCapabilities
	CredentialResolver          = llm.CredentialResolver
	ResolvedCredential          = llm.ResolvedCredential
	ToolDefinition              = tools.Definition
	ExecutionDescription        = tools.ExecutionDescription
	ExecutorOption              = tools.ExecutorOption
	ResourceScheduler           = tools.ResourceScheduler
	Operations                  = tools.Operations
	BuiltinOptions              = tools.BuiltinOptions
	OutputLimits                = tools.OutputLimits
	Preview                     = tools.Preview
	BackendCapabilities         = agent.BackendCapabilities
	BackendCapabilityReporter   = agent.BackendCapabilityReporter
	FileOperations              = agent.FileOperations
	FileEffect                  = agent.FileEffect
	ListRequest                 = agent.ListRequest
	ListResult                  = agent.ListResult
	FileEntry                   = agent.FileEntry
	ReadRequest                 = agent.ReadRequest
	ReadResult                  = agent.ReadResult
	SearchRequest               = agent.SearchRequest
	SearchResult                = agent.SearchResult
	SearchMatch                 = agent.SearchMatch
	ProcessOperations           = agent.ProcessOperations
	ProgressSink                = agent.ProgressSink
	ArtifactStore               = agent.ArtifactStore
	OutputArtifactStore         = agent.OutputArtifactStore
	OutputArtifactBinding       = agent.OutputArtifactBinding
	OutputArtifactInput         = agent.OutputArtifactInput
	OutputArtifactRead          = agent.OutputArtifactRead
	OutputRedactor              = agent.OutputRedactor
	ToolOutputProjection        = agent.ToolOutputProjection
	TodoOperations              = agent.TodoOperations
	AuthorizedExecution         = agent.AuthorizedExecution
	AuthorizedProcess           = agent.AuthorizedProcess
	AuthorizedFileWrite         = agent.AuthorizedFileWrite
	AuthorizedFileEdit          = agent.AuthorizedFileEdit
	ArtifactInput               = agent.ArtifactInput
	ArtifactRead                = agent.ArtifactRead
	ArtifactRef                 = agent.ArtifactRef
	ProcessProgress             = agent.ProcessProgress
	ProcessObservation          = agent.ProcessObservation
	ExecutionRef                = agent.ExecutionRef
	StopObservation             = agent.StopObservation
	TodoEffect                  = agent.TodoEffect
	AuthorizedTodo              = agent.AuthorizedTodo
	Executor                    = tools.Executor
	Outcome                     = tools.Outcome
	AgentDeps                   = einorun.Deps
	SessionStore                = store.Store
	Commit                      = store.Commit
	ExpectedCommit              = store.ExpectedCommit
	CommitReceipt               = store.CommitReceipt
	Header                      = store.Header
	StoredSession               = store.StoredSession
	CommitReader                = store.CommitReader
	Record                      = store.Record
	BranchUpdate                = store.BranchUpdate
)

func CreateWorkflowAgent(ctx context.Context, opts WorkflowOptions) (*WorkflowAgent, error) {
	return workflowagent.CreateWorkflowAgent(ctx, opts)
}

func OpenWorkflowAgent(ctx context.Context, opts WorkflowOptions) (*WorkflowAgent, error) {
	return workflowagent.OpenWorkflowAgent(ctx, opts)
}

func CreateAgentSession(ctx context.Context, opts SessionOptions) (*AgentSession, error) {
	return codeagent.CreateAgentSession(ctx, opts)
}

func OpenAgentSession(ctx context.Context, opts SessionOptions) (*AgentSession, error) {
	return codeagent.OpenAgentSession(ctx, opts)
}

// WorkflowFormatV1 is the accepted unified workflow format; WorkflowModelBinding
// is the trusted default model binding used by startup workflow inventories.
const (
	WorkflowFormatV1     = workflowagent.WorkflowFormatV1
	WorkflowModelBinding = workflowagent.WorkflowModelBinding
)

func NewError(code, message string) *Error { return product.NewError(code, message) }

func Errorf(code, format string, args ...any) *Error { return product.Errorf(code, format, args...) }

func AsError(err error) (*Error, bool) { return product.AsError(err) }

func DefaultLimits() Limits { return config.DefaultLimits() }

func NewBudget(limits Limits) *BudgetLedger    { return agent.NewBudget(limits) }
func NewResourceScheduler() *ResourceScheduler { return tools.NewResourceScheduler() }

func NewCatalog(resolver CredentialResolver) *Catalog { return llm.NewCatalog(resolver) }

func NewBuiltinDefinitions(options BuiltinOptions) []ToolDefinition {
	return tools.NewBuiltinDefinitions(options)
}

func DefaultOutputLimits() OutputLimits { return tools.DefaultOutputLimits() }
func PreviewHead(text string, maxLines, maxBytes int) Preview {
	return tools.PreviewHead(text, maxLines, maxBytes)
}
func PreviewTail(text string, maxLines, maxBytes int) Preview {
	return tools.PreviewTail(text, maxLines, maxBytes)
}

func NewExecutor(gen string, defs []ToolDefinition, sink ExecutionSink, auth ToolAuthorizer, budg *BudgetLedger, options ...ExecutorOption) (*Executor, error) {
	return tools.NewExecutor(gen, defs, sink, auth, budg, options...)
}

func NewAgent(ctx context.Context, deps AgentDeps) (adk.TypedResumableAgent[*schema.AgenticMessage], error) {
	return einorun.NewAgent(ctx, deps)
}

// NewPipelineTool adapts the controlled executor for AgentDeps.Tools.
func NewPipelineTool(info *schema.ToolInfo, exec *Executor, scope ExecutionScope) tool.InvokableTool {
	return einorun.NewPipelineTool(info, exec, scope)
}

// WithExecutionScope shares turn scope updates with tools and runner hooks.
func WithExecutionScope(ctx context.Context, scope ExecutionScope) context.Context {
	return einorun.WithExecutionScope(ctx, scope)
}

// ScopeFromContext returns the current execution scope, or fallback if absent.
func ScopeFromContext(ctx context.Context, fallback ExecutionScope) ExecutionScope {
	return einorun.ScopeFromContext(ctx, fallback)
}

// FinishAfterTools supplies adk.WithAfterToolCallsHook for standalone execution.
// It finishes the tool turn and returns ErrControlledStop when the boundary stops.
func FinishAfterTools(boundary BoundaryController, scope ExecutionScope) func(context.Context) error {
	return einorun.FinishAfterTools(boundary, scope)
}
