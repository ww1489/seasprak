package state

import (
	"context"
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

type OperationCommand struct {
	Principal        string
	Kind             string
	Target           string
	IdempotencyKey   string
	ExpectedRevision uint64 // Session commit revision at acceptance.
	Content          json.RawMessage
}
type OperationReceipt struct {
	OperationID    string `json:"operationId"`
	State          string `json:"state"`
	Target         string `json:"target"`
	AcceptedCommit uint64 `json:"acceptedCommit"`
}
type Operation struct {
	Receipt   OperationReceipt `json:"receipt"` // Immutable original accepted response.
	Principal string           `json:"principal"`
	SessionID string           `json:"sessionId"`
	Kind      string           `json:"kind"`
	Key       string           `json:"key"`
	Digest    string           `json:"digest"`
	Revision  uint64           `json:"revision"`
	State     string           `json:"state"`
	ResultRef string           `json:"resultRef,omitempty"`
	ErrorRef  string           `json:"errorRef,omitempty"`
}
type OperationStatus struct {
	OperationReceipt
	Revision  uint64
	ResultRef string
	ErrorRef  string
}

func (m *Manager) AcceptOperation(ctx context.Context, cmd OperationCommand) (OperationReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cmd.Kind == "" || cmd.Target == "" {
		return OperationReceipt{}, product.NewError(product.CodeInvalidArgument, "operation kind and target are required")
	}
	content, err := json.Marshal(struct {
		Content          json.RawMessage `json:"content"`
		ExpectedRevision uint64          `json:"expectedRevision"`
	}{cmd.Content, cmd.ExpectedRevision})
	if err != nil {
		return OperationReceipt{}, product.NewError(product.CodeInvalidArgument, "invalid operation content")
	}
	digest, err := digestCommand(agent.InputCommand{Kind: cmd.Kind, TargetTraceID: cmd.Target, Content: content})
	if err != nil {
		return OperationReceipt{}, err
	}
	// Check durable receipts before any revision or operation-state precondition.
	if cmd.IdempotencyKey != "" {
		for _, old := range m.view.Operations {
			if old.Principal == cmd.Principal && old.SessionID == m.sessionID && old.Kind == cmd.Kind && old.Key == cmd.IdempotencyKey {
				if old.Digest != digest {
					return OperationReceipt{}, product.NewError(product.CodeIdempotencyConflict, "key already belongs to different operation content")
				}
				return old.Receipt, nil
			}
		}
	}
	if cmd.ExpectedRevision != m.view.LastSeq {
		return OperationReceipt{}, product.NewError(product.CodeStateConflict, "session revision changed")
	}
	receipt := OperationReceipt{OperationID: agent.MustID(), State: "accepted", Target: cmd.Target, AcceptedCommit: m.view.LastSeq + 1}
	op := Operation{Receipt: receipt, Principal: cmd.Principal, SessionID: m.sessionID, Kind: cmd.Kind, Key: cmd.IdempotencyKey, Digest: digest, Revision: 1, State: "accepted"}
	if _, err := m.commit(ctx, []store.Record{record("operation", receipt.OperationID, op)}, nil, nil); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (m *Manager) GetOperation(id string) (OperationStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.view.Operations[id]
	if !ok {
		return OperationStatus{}, product.NewError(product.CodeNotFound, "operation not found")
	}
	result := OperationStatus{OperationReceipt: op.Receipt, Revision: op.Revision, ResultRef: op.ResultRef, ErrorRef: op.ErrorRef}
	result.State = op.State
	return result, nil
}

func (m *Manager) TransitionOperation(ctx context.Context, id string, expectedRevision uint64, next, resultRef, errorRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.view.Operations[id]
	if !ok {
		return product.NewError(product.CodeNotFound, "operation not found")
	}
	if op.Revision != expectedRevision || !operationTransition(op.State, next) {
		return product.NewError(product.CodeStateConflict, "invalid operation transition or revision")
	}
	op.Revision++
	op.State = next
	op.ResultRef = resultRef
	op.ErrorRef = errorRef
	_, err := m.commit(ctx, []store.Record{record("operation", id, op)}, nil, nil)
	return err
}
func operationTransition(old, next string) bool {
	switch old {
	case "accepted":
		return next == "running" || next == "completed" || next == "failed" || next == "cancelled"
	case "running":
		return next == "completed" || next == "failed" || next == "cancelled"
	default:
		return false
	}
}
func applyOperation(v *View, r store.Record) error {
	var op Operation
	if err := json.Unmarshal(r.Payload, &op); err != nil {
		return err
	}
	if op.Receipt.OperationID != r.ID || r.ID == "" || op.Receipt.State != "accepted" || op.Receipt.AcceptedCommit == 0 || op.Kind == "" || op.SessionID == "" || op.Digest == "" {
		return product.NewError(product.CodeIncompatibleVersion, "invalid operation record")
	}
	old, ok := v.Operations[r.ID]
	if !ok {
		if op.Revision != 1 || op.State != "accepted" || op.Receipt.AcceptedCommit != v.LastSeq+1 {
			return product.NewError(product.CodeStateConflict, "operation must start accepted")
		}
		if op.Key != "" {
			for _, existing := range v.Operations {
				if existing.Principal == op.Principal && existing.SessionID == op.SessionID && existing.Kind == op.Kind && existing.Key == op.Key {
					return product.NewError(product.CodeIdempotencyConflict, "duplicate operation key")
				}
			}
		}
	} else if op.Receipt != old.Receipt || op.Principal != old.Principal || op.SessionID != old.SessionID || op.Kind != old.Kind || op.Key != old.Key || op.Digest != old.Digest || op.Revision != old.Revision+1 || !operationTransition(old.State, op.State) {
		return product.NewError(product.CodeStateConflict, "invalid persisted operation transition")
	}
	if v.Operations == nil {
		v.Operations = map[string]Operation{}
	}
	v.Operations[r.ID] = op
	return nil
}
