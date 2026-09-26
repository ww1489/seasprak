package agent

import (
	"context"
	"encoding/json"

	"github.com/ww1489/seasprak/internal/llm"
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
	// Budget is the candidate usage committed atomically with a tool intent.
	Budget *Usage
}

// ModelAttemptIdentity is allocated before requesting a model and retained in
// the request context until its sole terminal commit.
type ModelAttemptIdentity struct {
	ID                 string `json:"id"`
	ModelCallID        string `json:"modelCallId"`
	MessageID          string `json:"messageId"`
	StreamID           string `json:"streamId"`
	ModelConfigVersion string `json:"modelConfigVersion,omitempty"`
}

// ModelAttemptDetails is terminal evidence, not a second budget ledger. Each
// usage item is the last cumulative observation of one physical request.
type ModelAttemptDetails struct {
	FailureCode          string              `json:"failureCode,omitempty"`
	FailureReason        string              `json:"failureReason,omitempty"`
	RefusalReason        string              `json:"refusalReason,omitempty"`
	OriginalFinishReason string              `json:"originalFinishReason,omitempty"`
	Usage                []ModelRequestUsage `json:"usage,omitempty"`
}
type ModelRequestUsage struct {
	Request  llm.TransportRequest `json:"request"`
	Snapshot llm.UsageSnapshot    `json:"snapshot"`
}

// ModelStreamSnapshot is a temporary allowlisted presentation, not model history.
// Provider extensions and reasoning signatures never enter this payload.
type ModelStreamSnapshot struct {
	AttemptID string             `json:"attemptId"`
	MessageID string             `json:"messageId"`
	StreamID  string             `json:"streamId"`
	ChunkSeq  uint64             `json:"chunkSeq"`
	Blocks    []ModelStreamBlock `json:"blocks"`
}
type ModelStreamBlock struct {
	BlockIndex int    `json:"blockIndex"`
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	CallID     string `json:"callId,omitempty"`
	Name       string `json:"name,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
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
	ID                string
	TraceID           string
	InvocationID      string
	Ended             bool
	CallIDs           []string
	TransportRequests int
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
