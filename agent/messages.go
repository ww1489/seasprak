package agent

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/model"
)

type MessageKind string

const (
	KindUser              MessageKind = "user"
	KindAssistant         MessageKind = "assistant"
	KindToolResult        MessageKind = "tool_result"
	KindCustom            MessageKind = "custom"
	KindCommand           MessageKind = "command"
	KindCompactionSummary MessageKind = "compaction_summary"
	KindBranchSummary     MessageKind = "branch_summary"
	KindOpaque            MessageKind = "opaque"
)

type MessageStatus string

const (
	StatusComplete   MessageStatus = "complete"
	StatusIncomplete MessageStatus = "incomplete"
)

type SourceKind string

const (
	SourceHuman        SourceKind = "human"
	SourceDirectParent SourceKind = "direct_parent"
	SourceExtension    SourceKind = "extension"
	SourceTool         SourceKind = "tool"
	SourceModel        SourceKind = "model"
	SourceResource     SourceKind = "resource"
	SourceImported     SourceKind = "imported"
)

type MessageScope struct {
	SessionID    string `json:"sessionId,omitempty"`
	TraceID      string `json:"traceId,omitempty"`
	TurnID       string `json:"turnId,omitempty"`
	InvocationID string `json:"invocationId,omitempty"`
	InputID      string `json:"inputId,omitempty"`
	ToolCallID   string `json:"toolCallId,omitempty"`
}

type SourceRef struct {
	Kind        SourceKind `json:"kind"`
	Description string     `json:"description,omitempty"`
}

type CustomMessage struct {
	CustomType string                 `json:"customType"`
	Content    *schema.AgenticMessage `json:"content,omitempty"`
	Details    json.RawMessage        `json:"details,omitempty"`
	Display    bool                   `json:"display"`
}

type SummaryMessage struct {
	Text string `json:"text"`
}

type CommandMessage struct {
	Name    string          `json:"name"`
	Content json.RawMessage `json:"content,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

type OpaqueMessage struct {
	Raw              json.RawMessage `json:"raw"`
	RequiredForModel bool            `json:"requiredForModel"`
}

// AgentMessage is a discriminated union. Kind selects exactly one payload.
type AgentMessage struct {
	ID       string                 `json:"id"`
	Kind     MessageKind            `json:"kind"`
	Scope    MessageScope           `json:"scope"`
	Source   SourceRef              `json:"source"`
	Status   MessageStatus          `json:"status"`
	Standard *schema.AgenticMessage `json:"standard,omitempty"`
	Custom   *CustomMessage         `json:"custom,omitempty"`
	Summary  *SummaryMessage        `json:"summary,omitempty"`
	Command  *CommandMessage        `json:"command,omitempty"`
	Opaque   *OpaqueMessage         `json:"opaque,omitempty"`
}

func (m AgentMessage) Validate() error {
	if m.ID == "" {
		return model.NewError(model.CodeInvalidArgument, "message id is required")
	}
	var n int
	if m.Standard != nil {
		n++
	}
	if m.Custom != nil {
		n++
	}
	if m.Summary != nil {
		n++
	}
	if m.Command != nil {
		n++
	}
	if m.Opaque != nil {
		n++
	}
	if n != 1 {
		return model.NewError(model.CodeInvalidArgument, "message must contain exactly one payload")
	}
	switch m.Kind {
	case KindUser, KindAssistant, KindToolResult:
		if m.Standard == nil {
			return model.Errorf(model.CodeInvalidArgument, "kind %s requires standard payload", m.Kind)
		}
	case KindCustom:
		if m.Custom == nil {
			return model.NewError(model.CodeInvalidArgument, "custom kind requires custom payload")
		}
	case KindCommand:
		if m.Command == nil {
			return model.NewError(model.CodeInvalidArgument, "command kind requires command payload")
		}
	case KindCompactionSummary, KindBranchSummary:
		if m.Summary == nil {
			return model.NewError(model.CodeInvalidArgument, "summary kind requires summary payload")
		}
	case KindOpaque:
		if m.Opaque == nil {
			return model.NewError(model.CodeInvalidArgument, "opaque kind requires opaque payload")
		}
	default:
		return model.Errorf(model.CodeInvalidArgument, "unknown message kind %s", m.Kind)
	}
	return nil
}

type EventScope struct {
	SessionID string `json:"sessionId"`
	TraceID   string `json:"traceId,omitempty"`
	TurnID    string `json:"turnId,omitempty"`
}

type Event struct {
	SchemaVersion int             `json:"schemaVersion"`
	Type          string          `json:"type"`
	Scope         EventScope      `json:"scope"`
	EventID       string          `json:"eventId,omitempty"`
	DurableSeq    *uint64         `json:"durableSeq,omitempty"`
	StreamID      string          `json:"streamId,omitempty"`
	ChunkSeq      *uint64         `json:"chunkSeq,omitempty"`
	OccurredAt    time.Time       `json:"occurredAt"`
	Payload       json.RawMessage `json:"payload"`
}

func (e Event) Validate() error {
	if e.SchemaVersion != 1 {
		return model.NewError(model.CodeIncompatibleVersion, "unsupported event schema")
	}
	if e.Type == "" || e.Scope.SessionID == "" {
		return model.NewError(model.CodeInvalidArgument, "event type and session are required")
	}
	return nil
}

type TargetAgent struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Generation string `json:"generation"`
}

type InputCommand struct {
	Kind           string          `json:"kind"`
	TargetTraceID  string          `json:"targetTraceId,omitempty"`
	TargetAgent    string          `json:"targetAgent,omitempty"`
	Content        json.RawMessage `json:"content"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	Principal      string          `json:"-"`
}

type InputReceipt struct {
	InputID        string      `json:"inputId"`
	TraceID        string      `json:"traceId"`
	ActualKind     string      `json:"actualKind"`
	TargetAgent    TargetAgent `json:"targetAgent"`
	State          string      `json:"state"`
	AcceptedCommit uint64      `json:"acceptedCommit"`
}

func MustID() string {
	id, err := model.NewID()
	if err != nil {
		panic(fmt.Sprintf("id: %v", err))
	}
	return id
}
