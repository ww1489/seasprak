package agent

import (
	"context"
	"encoding/json"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/model"
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

// TransformContext copies messages and drops incomplete failures.
// It does not delete an accepted tool-call group or change authorization.
func TransformContext(in []AgentMessage) ([]AgentMessage, error) {
	out := make([]AgentMessage, 0, len(in))
	for _, msg := range in {
		if err := msg.Validate(); err != nil {
			return nil, err
		}
		if msg.Status == StatusIncomplete {
			continue
		}
		if msg.Kind == KindOpaque && msg.Opaque.RequiredForModel {
			return nil, model.NewError(model.CodeUnsupportedCapability, "opaque message has no projection rule")
		}
		out = append(out, msg)
	}
	return out, nil
}

// ConvertToLLM projects already selected messages. It does not read the network or history.
func ConvertToLLM(in []AgentMessage) ([]*schema.AgenticMessage, error) {
	selected, err := TransformContext(in)
	if err != nil {
		return nil, err
	}
	out := make([]*schema.AgenticMessage, 0, len(selected))
	for _, msg := range selected {
		switch msg.Kind {
		case KindUser, KindAssistant, KindToolResult:
			out = append(out, msg.Standard)
		case KindCustom:
			if msg.Custom.Content == nil {
				return nil, model.NewError(model.CodeInvalidArgument, "custom content is required")
			}
			copied := *msg.Custom.Content
			copied.Role = schema.AgenticRoleTypeUser
			out = append(out, &copied)
		case KindCommand:
			text := msg.Command.Name
			out = append(out, schema.UserAgenticMessage(text))
		case KindCompactionSummary, KindBranchSummary:
			out = append(out, schema.UserAgenticMessage(msg.Summary.Text))
		case KindOpaque:
			continue
		default:
			return nil, model.Errorf(model.CodeInvalidArgument, "cannot project %s", msg.Kind)
		}
	}
	return out, nil
}
