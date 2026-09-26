package state

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// P2 records contain product facts, never framework checkpoint values or backend handles.
// Complex execution content lives here; FrozenCall and ToolObservation remain comparable.
type ModelAttempt struct {
	ID                 string               `json:"id"`
	ModelCallID        string               `json:"modelCallId"`
	MessageID          string               `json:"messageId"`
	StreamID           string               `json:"streamId"`
	Scope              agent.ExecutionScope `json:"scope"`
	Purpose            string               `json:"purpose"`
	Attempt            uint64               `json:"attempt"`
	TransportAttempt   uint64               `json:"transportAttempt"`
	State              string               `json:"state"`
	ModelConfigVersion string               `json:"modelConfigVersion"`
	FinishReason       string               `json:"finishReason,omitempty"`
	DiagnosticRef      string               `json:"diagnosticRef,omitempty"`
	UsageRef           string               `json:"usageRef,omitempty"`
}

// State keeps the historical JSON record names while the execution layer owns
// the single immutable descriptor used by preparation, policy and tickets.
type ExecutionResource = agent.ExecutionResource
type ExecutionMount = agent.ExecutionMount
type FrozenExecution = agent.FrozenExecution

type Interaction struct {
	ID              string               `json:"id"`
	Kind            string               `json:"kind"`
	Scope           agent.ExecutionScope `json:"scope"`
	CallID          string               `json:"callId,omitempty"`
	NodeExecutionID string               `json:"nodeExecutionId,omitempty"`
	ApprovalID      string               `json:"approvalId,omitempty"`
	Question        string               `json:"question"`
	Options         []string             `json:"options,omitempty"`
	PendingFields   []string             `json:"pendingFields,omitempty"`
	State           string               `json:"state"`
	ExpiresAt       time.Time            `json:"expiresAt"`
	CheckpointRef   string               `json:"checkpointRef,omitempty"`
	TargetRef       string               `json:"targetRef,omitempty"`
}
type Approval struct {
	ID                string               `json:"id"`
	InteractionID     string               `json:"interactionId"`
	CallID            string               `json:"callId"`
	Scope             agent.ExecutionScope `json:"scope"`
	FrozenExecutionID string               `json:"frozenExecutionId"`
	FrozenHash        string               `json:"frozenHash"`
	GrantRef          string               `json:"grantRef,omitempty"`
	State             string               `json:"state"`
	Principal         string               `json:"principal,omitempty"`
	ExpiresAt         time.Time            `json:"expiresAt"`
}
type CheckpointRef struct {
	ID                     string               `json:"id"`
	BlobHash               string               `json:"blobHash"`
	BlobSize               int64                `json:"blobSize"`
	Scope                  agent.ExecutionScope `json:"scope"`
	Target                 agent.TargetAgent    `json:"target"`
	Input                  agent.InputRef       `json:"input,omitempty"`
	UnfinishedTurnIDs      []string             `json:"unfinishedTurnIds,omitempty"`
	CallIDs                []string             `json:"callIds,omitempty"`
	ModelConfigVersion     string               `json:"modelConfigVersion"`
	ThinkingRef            string               `json:"thinkingRef,omitempty"`
	SelectionRevision      uint64               `json:"selectionRevision"`
	ProjectionRevision     uint64               `json:"projectionRevision"`
	LeafID                 string               `json:"leafId"`
	HistoryCommit          uint64               `json:"historyCommit"`
	ExtensionStateCommit   uint64               `json:"extensionStateCommit"`
	InteractionIDs         []string             `json:"interactionIds,omitempty"`
	EinoVersion            string               `json:"einoVersion"`
	CodecVersion           string               `json:"codecVersion"`
	BuildCompatibility     string               `json:"buildCompatibility"`
	EnvironmentFingerprint string               `json:"environmentFingerprint"`
	ManifestHash           string               `json:"manifestHash"`
}

// Records is a finite batch of immutable facts. Later execution steps own lifecycle
// transitions; saving these facts alone grants no approval or resume permission.
type Records struct {
	ModelAttempts    []ModelAttempt
	FrozenExecutions []FrozenExecution
	Interactions     []Interaction
	Approvals        []Approval
	Checkpoints      []CheckpointRef
}

func (m *Manager) SaveRecords(ctx context.Context, expectedRevision uint64, records Records) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if expectedRevision != m.view.LastSeq {
		return product.NewError(product.CodeStateConflict, "session revision changed")
	}
	var controls []store.Record
	for _, r := range records.ModelAttempts {
		controls = append(controls, record("model_attempt", r.ID, r))
	}
	for _, r := range records.FrozenExecutions {
		controls = append(controls, record("frozen_execution", r.ID, r))
	}
	for _, r := range records.Interactions {
		controls = append(controls, record("interaction", r.ID, r))
	}
	for _, r := range records.Approvals {
		controls = append(controls, record("approval", r.ID, r))
	}
	for _, r := range records.Checkpoints {
		controls = append(controls, record("checkpoint_ref", r.ID, r))
	}
	if len(controls) == 0 {
		return product.NewError(product.CodeInvalidArgument, "records are required")
	}
	_, err := m.commit(ctx, controls, nil, nil)
	return err
}

// putImmutable is a map insertion check, not a codec/type registry. The replay
// switch below explicitly enumerates every supported critical record kind.
func putImmutable[T any](items *map[string]T, id, payloadID string, value T) error {
	if id == "" || id != payloadID {
		return product.NewError(product.CodeIncompatibleVersion, "record identity mismatch")
	}
	if old, ok := (*items)[id]; ok && !reflect.DeepEqual(old, value) {
		return product.NewError(product.CodeStateConflict, "immutable record already exists")
	}
	if *items == nil {
		*items = make(map[string]T)
	}
	(*items)[id] = value
	return nil
}

func applyP2Record(v *View, r store.Record) error {
	switch r.Type {
	case "model_attempt":
		var value ModelAttempt
		if err := json.Unmarshal(r.Payload, &value); err != nil {
			return err
		}
		return putImmutable(&v.ModelAttempts, r.ID, value.ID, value)
	case "frozen_execution":
		var value FrozenExecution
		if err := json.Unmarshal(r.Payload, &value); err != nil {
			return err
		}
		return putImmutable(&v.FrozenExecutions, r.ID, value.ID, value)
	case "interaction":
		var value Interaction
		if err := json.Unmarshal(r.Payload, &value); err != nil {
			return err
		}
		return putImmutable(&v.Interactions, r.ID, value.ID, value)
	case "approval":
		var value Approval
		if err := json.Unmarshal(r.Payload, &value); err != nil {
			return err
		}
		return putImmutable(&v.Approvals, r.ID, value.ID, value)
	case "checkpoint_ref":
		var value CheckpointRef
		if err := json.Unmarshal(r.Payload, &value); err != nil {
			return err
		}
		return putImmutable(&v.Checkpoints, r.ID, value.ID, value)
	default:
		return product.NewError(product.CodeIncompatibleVersion, "unknown required P2 record")
	}
}
