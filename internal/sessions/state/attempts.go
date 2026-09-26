package state

import (
	"context"
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// ModelAttemptTransition is an append-only terminal fact. The started record is
// retained unchanged; revision 1 belongs to that original identity.
type ModelAttemptTransition struct {
	ID               string `json:"id"`
	AttemptID        string `json:"attemptId"`
	ExpectedRevision uint64 `json:"expectedRevision"`
	Revision         uint64 `json:"revision"`
	State            string `json:"state"`
	FinishReason     string `json:"finishReason,omitempty"`
	UsageRef         string `json:"usageRef,omitempty"`
	DiagnosticRef    string `json:"diagnosticRef,omitempty"`
}

// Both references point to this one immutable terminal evidence record. Usage
// remains grouped by physical request; it is never added to the budget ledger.
type ModelAttemptDetailsRecord struct {
	ID        string `json:"id"`
	AttemptID string `json:"attemptId"`
	agent.ModelAttemptDetails
}

func applyAttemptDetails(v *View, r store.Record) error {
	var details ModelAttemptDetailsRecord
	if err := json.Unmarshal(r.Payload, &details); err != nil {
		return err
	}
	initial, ok := v.ModelAttempts[details.AttemptID]
	if !ok {
		return product.NewError(product.CodeStateConflict, "attempt details have no initial record")
	}
	if _, ended := v.AttemptResults[details.AttemptID]; ended {
		return product.NewError(product.CodeStateConflict, "attempt details arrived after terminal")
	}
	seen := map[uint64]bool{}
	for _, item := range details.Usage {
		request := item.Request
		if request.AttemptID != initial.ID || request.ModelCallID != initial.ModelCallID || request.TransportAttempt == 0 || seen[request.TransportAttempt] {
			return product.NewError(product.CodeStateConflict, "attempt usage identity mismatch")
		}
		seen[request.TransportAttempt] = true
	}
	return putImmutable(&v.AttemptDetails, r.ID, details.ID, details)
}

func applyAttemptTransition(v *View, r store.Record) error {
	var result ModelAttemptTransition
	if err := json.Unmarshal(r.Payload, &result); err != nil {
		return err
	}
	initial, ok := v.ModelAttempts[result.AttemptID]
	if !ok || initial.State != "started" || result.ID == "" || result.ID != r.ID || result.ExpectedRevision != 1 || result.Revision != 2 {
		return product.NewError(product.CodeStateConflict, "invalid model attempt transition")
	}
	if _, exists := v.AttemptResults[result.AttemptID]; exists {
		return product.NewError(product.CodeStateConflict, "model attempt already ended")
	}
	switch result.State {
	case "accepted", "failed", "incomplete", "aborted":
	default:
		return product.NewError(product.CodeStateConflict, "invalid model attempt outcome")
	}
	for _, ref := range []string{result.UsageRef, result.DiagnosticRef} {
		if ref != "" && v.AttemptDetails[ref].AttemptID != result.AttemptID {
			return product.NewError(product.CodeStateConflict, "attempt details reference mismatch")
		}
	}
	if v.AttemptResults == nil {
		v.AttemptResults = map[string]ModelAttemptTransition{}
	}
	v.AttemptResults[result.AttemptID] = result
	return nil
}

// SaveAttemptResult is the sole terminal transaction for a registered attempt.
// Accepted tools and their assistant message become visible together.
func (m *Manager) SaveAttemptResult(ctx context.Context, scope agent.ExecutionScope, id, status, finish string, msg *agent.AgentMessage, calls []agent.ToolRecord, details ...agent.ModelAttemptDetails) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	initial, ok := m.view.ModelAttempts[id]
	if !ok || initial.Scope != scope {
		return product.NewError(product.CodeStateConflict, "model attempt scope mismatch")
	}
	if _, ended := m.view.AttemptResults[id]; ended {
		return product.NewError(product.CodeStateConflict, "model attempt already ended")
	}
	if msg != nil && (msg.ID != initial.MessageID || msg.Scope.SessionID != scope.SessionID || msg.Scope.TraceID != scope.TraceID || msg.Scope.TurnID != scope.TurnID || msg.Scope.InvocationID != scope.InvocationID) {
		return product.NewError(product.CodeStateConflict, "model candidate identity mismatch")
	}
	evidence := ModelAttemptDetailsRecord{ID: agent.MustID(), AttemptID: id}
	if len(details) > 1 {
		return product.NewError(product.CodeInvalidArgument, "one attempt details record is required")
	}
	if len(details) == 1 {
		evidence.ModelAttemptDetails = details[0]
	}
	result := ModelAttemptTransition{ID: agent.MustID(), AttemptID: id, ExpectedRevision: 1, Revision: 2, State: status, FinishReason: finish, UsageRef: evidence.ID, DiagnosticRef: evidence.ID}
	controls := []store.Record{record("model_attempt_details", evidence.ID, evidence), record("model_attempt_transition", result.ID, result)}
	if status == "accepted" {
		if msg == nil || msg.Status != agent.StatusComplete {
			return product.NewError(product.CodeInvalidArgument, "accepted attempt requires complete message")
		}
		for _, call := range calls {
			if call.Scope != scope {
				return product.NewError(product.CodeStateConflict, "attempt tool scope mismatch")
			}
		}
		return m.saveAssistant(ctx, *msg, calls, controls)
	}
	if len(calls) != 0 {
		return product.NewError(product.CodeInvalidArgument, "failed attempt cannot accept tools")
	}
	events := []agent.Event{m.event("model.attempt_failed", scope.TraceID, scope.TurnID, result)}
	var entries []store.Record
	if msg != nil {
		if msg.Status != agent.StatusIncomplete {
			return product.NewError(product.CodeInvalidArgument, "failed candidate must be incomplete")
		}
		if err := msg.Validate(); err != nil {
			return err
		}
		entry := record("message", msg.ID, *msg)
		entry.ParentID = m.view.LeafID
		entries = append(entries, entry)
		events = append(events, m.event("message.finalized", scope.TraceID, scope.TurnID, *msg))
	}
	_, err := m.commit(ctx, controls, entries, events)
	return err
}
