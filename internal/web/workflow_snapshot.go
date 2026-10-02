package web

import (
	"encoding/base64"
	"encoding/json"
	"strconv"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/workflowagent"
)

type workflowSnapshotDTO struct {
	RunID             string                   `json:"runId"`
	DefinitionName    string                   `json:"definitionName"`
	DefinitionVersion string                   `json:"definitionVersion"`
	State             string                   `json:"state"`
	Revision          uint64                   `json:"revision"`
	Cursor            string                   `json:"cursor"`
	DurableSeq        string                   `json:"durableSeq"`
	InstanceID        string                   `json:"instanceId"`
	ExecutionStopped  bool                     `json:"executionStopped"`
	CanResume         bool                     `json:"canResume"`
	WorkflowNodes     []workflowRunNodeDTO     `json:"workflowNodes"`
	Interactions      []workflowInteractionDTO `json:"interactions"`
	ErrorCode         string                   `json:"errorCode,omitempty"`
	FailedNode        string                   `json:"failedNode,omitempty"`
	Result            json.RawMessage          `json:"result,omitempty"`
}

type workflowRunNodeDTO struct {
	NodeExecutionID string `json:"nodeExecutionId"`
	NodeID          string `json:"nodeId"`
	Kind            string `json:"kind"`
	State           string `json:"state"`
}

type workflowInteractionDTO struct {
	InteractionID   string   `json:"interactionId"`
	NodeExecutionID string   `json:"nodeExecutionId"`
	Question        string   `json:"question"`
	Options         []string `json:"options"`
	InstanceID      string   `json:"instanceId"`
}

func encodeWorkflowCursor(rid string, position uint64) string {
	raw, _ := json.Marshal([]any{2, "workflow", rid, strconv.FormatUint(position, 10)})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// projectWorkflowSnapshot is a value-only HTTP boundary. Runtime state,
// arguments, node results and authorization metadata are never serialized.
func projectWorkflowSnapshot(source workflowagent.WorkflowSnapshot) (workflowSnapshotDTO, error) {
	if storage.ValidateResourceID(source.RunID) != nil || source.Revision > (1<<53)-1 || source.Cursor != encodeWorkflowCursor(source.RunID, source.DurableSeq) {
		return workflowSnapshotDTO{}, product.NewError(product.CodeInternal, "workflow snapshot cannot be projected")
	}
	out := workflowSnapshotDTO{
		RunID: source.RunID, DefinitionName: source.DefinitionName, DefinitionVersion: source.DefinitionVersion,
		State: source.State, Revision: source.Revision, Cursor: source.Cursor, DurableSeq: strconv.FormatUint(source.DurableSeq, 10),
		InstanceID: source.InstanceID, ExecutionStopped: source.ExecutionStopped, CanResume: source.CanResume,
		WorkflowNodes: []workflowRunNodeDTO{}, Interactions: []workflowInteractionDTO{},
		ErrorCode: workflowPublicErrorCode(source.ErrorCode), FailedNode: source.FailedNode,
	}
	for _, id := range sortedKeys(source.WorkflowNodes) {
		node := source.WorkflowNodes[id]
		out.WorkflowNodes = append(out.WorkflowNodes, workflowRunNodeDTO{NodeExecutionID: node.ID, NodeID: node.NodeID, Kind: node.Kind, State: node.State})
	}
	if source.State == "paused" && source.ExecutionStopped {
		for _, id := range sortedKeys(source.Interactions) {
			question := source.Interactions[id]
			if question.InstanceID != source.InstanceID || question.State != "ready" {
				continue
			}
			options := []string{}
			for _, option := range question.Options {
				if option == "allowed-once" || option == "rejected" || option == "cancelled" {
					options = append(options, option)
				}
			}
			out.Interactions = append(out.Interactions, workflowInteractionDTO{InteractionID: question.ID, NodeExecutionID: question.NodeExecutionID, Question: question.Question, Options: options, InstanceID: question.InstanceID})
		}
	}
	if source.State == "completed" && len(source.Result) != 0 {
		if !json.Valid(source.Result) {
			return workflowSnapshotDTO{}, product.NewError(product.CodeInternal, "workflow result cannot be projected")
		}
		out.Result = append(json.RawMessage(nil), source.Result...)
	}
	return out, nil
}

func workflowPublicErrorCode(code string) string {
	switch code {
	case "", product.CodeInvalidArgument, product.CodeUnauthenticated, product.CodePermissionDenied, product.CodeNotFound,
		product.CodeStateConflict, product.CodeIdempotencyConflict, product.CodeUnsupportedCapability, product.CodeBudgetExhausted,
		product.CodeStorageUnavailable, product.CodeIncompatibleVersion, product.CodeIncompatibleResume, product.CodeReconciliationRequired,
		product.CodeResyncRequired, product.CodeResourceUnavailable, product.CodeInternal:
		return code
	default:
		return product.CodeInternal
	}
}
