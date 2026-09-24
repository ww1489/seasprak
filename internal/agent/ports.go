package agent

import (
	"context"
	"encoding/json"
)

// ScopeSnapshot is the fixed range used to build one model request.
type ScopeSnapshot struct {
	SessionID          string
	BranchID           string
	LeafID             string
	TraceID            string
	TurnID             string
	InvocationID       string
	Target             TargetAgent
	SelectionRevision  uint64
	ProjectionRevision uint64
	ModelConfigVersion string
	ConsumedInputIDs   []string
}

type ExecutionScope struct {
	SessionID          string
	BranchID           string
	TraceID            string
	InvocationID       string
	ParentInvocationID string
	Generation         string
	ExecutionID        string
	TurnID             string
}

type Fact struct {
	Kind    string
	Payload json.RawMessage
}

// ExecutionSink commits execution facts. Implementations must not run the agent.
type ExecutionSink interface {
	CommitFact(ctx context.Context, scope ExecutionScope, fact Fact) error
}

// InputSource returns inputs already assigned to this execution segment.
type InputSource interface {
	Take(ctx context.Context, scope ExecutionScope, kind string) (InputRef, *AgentMessage, bool, error)
}

type Decision string

const (
	DecisionAllow  Decision = "allow"
	DecisionDeny   Decision = "deny"
	DecisionCancel Decision = "cancel"
	DecisionAsk    Decision = "ask"
)

type TurnRecord struct {
	ID           string
	TraceID      string
	InvocationID string
	Ended        bool
	CallIDs      []string
}

type ToolObservation struct {
	Status     string
	Content    string
	SideEffect string
	Executed   bool
}

type ToolRecord struct {
	Call        FrozenCall
	Scope       ExecutionScope
	Claimed     bool
	Observation *ToolObservation
}

// ToolCallSource resolves accepted calls and prevents replaying an occupied execution.
type ToolCallSource interface {
	LookupTool(context.Context, ExecutionScope, string) (ToolRecord, error)
}

type FrozenCall struct {
	ProviderCallID string
	CallID         string
	Name           string
	Arguments      string
	Generation     string
	Hash           string
}

// ToolAuthorizer judges a frozen call. Ask is rejected until approval exists.
type ToolAuthorizer interface {
	Authorize(ctx context.Context, call FrozenCall) (Decision, error)
}

type TurnPlan struct {
	TurnID            string
	SelectionRevision uint64
}

type TurnFact struct {
	TurnID     string
	HasTools   bool
	Stop       bool
	StopReason string
}

// BoundaryController applies the product turn boundary without owning the model loop.
type BoundaryController interface {
	PrepareNextTurn(ctx context.Context, scope ExecutionScope) (TurnPlan, error)
	ShouldStop(ctx context.Context, scope ExecutionScope) (bool, string, error)
	FinishTurn(ctx context.Context, scope ExecutionScope, fact TurnFact) error
	TakeSteering(ctx context.Context, scope ExecutionScope) (*AgentMessage, error)
}

// InputRef is the only value stored in the TurnLoop buffer.
type InputRef struct {
	InputID string
	TraceID string
	Kind    string
}
