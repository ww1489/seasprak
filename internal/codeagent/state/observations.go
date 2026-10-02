package state

import (
	"context"
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

type ObservationRevision struct {
	ID               string                `json:"id"`
	CallID           string                `json:"callId"`
	Version          uint64                `json:"version"`
	PreviousID       string                `json:"previousId,omitempty"`
	Observation      agent.ToolObservation `json:"observation"`
	StartEvidenceRef string                `json:"startEvidenceRef,omitempty"`
	ExitRef          string                `json:"exitRef,omitempty"`
	CancellationRef  string                `json:"cancellationRef,omitempty"`
	DetailsRef       string                `json:"detailsRef,omitempty"`
	ArtifactRefs     []string              `json:"artifactRefs,omitempty"`
	FileFactsRef     string                `json:"fileFactsRef,omitempty"`
}
type Reconciliation struct {
	ID                   string   `json:"id"`
	OperationID          string   `json:"operationId"`
	CallID               string   `json:"callId"`
	ObservationID        string   `json:"observationId"`
	ObservationVersion   uint64   `json:"observationVersion"`
	NewObservationID     string   `json:"newObservationId"`
	GrantRef             string   `json:"grantRef,omitempty"`
	QueryID              string   `json:"queryId,omitempty"`
	TrustedNoStart       bool     `json:"trustedNoStart,omitempty"`
	EvidenceRefs         []string `json:"evidenceRefs"`
	EvidenceSource       string   `json:"evidenceSource"`
	ConfirmedEffects     []string `json:"confirmedEffects,omitempty"`
	RemainingUnknown     []string `json:"remainingUnknown,omitempty"`
	ConflictRestrictions []string `json:"conflictRestrictions,omitempty"`
	CanResume            bool     `json:"canResume"`
	ResumeReason         string   `json:"resumeReason"`
}

func (v View) latestObservation(callID string) (ObservationRevision, bool) {
	var latest ObservationRevision
	for _, r := range v.Observations {
		if r.CallID == callID && r.Version > latest.Version {
			latest = r
		}
	}
	return latest, latest.Version != 0
}
func (m *Manager) LatestObservation(callID string) (ObservationRevision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.view.latestObservation(callID)
	if !ok {
		return ObservationRevision{}, product.NewError(product.CodeNotFound, "observation not found")
	}
	return clone(r), nil
}

// AppendObservation records new evidence without replacing the original ToolRecord.
// This storage method does not release claims, run a query, or resume execution.
func (m *Manager) AppendObservation(ctx context.Context, expectedVersion uint64, next ObservationRevision, reconciliation *Reconciliation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, _ := m.view.latestObservation(next.CallID)
	if old.Version != expectedVersion {
		return product.NewError(product.CodeStateConflict, "observation revision changed")
	}
	controls := []store.Record{record("observation_revision", next.ID, next)}
	if reconciliation != nil {
		if reconciliation.NewObservationID != next.ID {
			return product.NewError(product.CodeStateConflict, "reconciliation result mismatch")
		}
		controls = append(controls, record("reconciliation", reconciliation.ID, *reconciliation))
	}
	_, err := m.commit(ctx, controls, nil, nil)
	return err
}

func applyObservation(v *View, r store.Record) error {
	var next ObservationRevision
	if err := json.Unmarshal(r.Payload, &next); err != nil {
		return err
	}
	if next.ID == "" || next.ID != r.ID {
		return product.NewError(product.CodeIncompatibleVersion, "observation identity mismatch")
	}
	if _, ok := v.Calls[next.CallID]; !ok {
		return product.NewError(product.CodeStateConflict, "observation call not found")
	}
	old, _ := v.latestObservation(next.CallID)
	if next.Version != old.Version+1 || next.PreviousID != old.ID {
		return product.NewError(product.CodeStateConflict, "observation does not extend current revision")
	}
	if old.Observation.SideEffect == "confirmed" && (!next.Observation.Executed || next.Observation.SideEffect != "confirmed") {
		return product.NewError(product.CodeStateConflict, "confirmed execution facts cannot be erased")
	}
	if _, ok := v.Observations[next.ID]; ok {
		return product.NewError(product.CodeStateConflict, "observation cannot be overwritten")
	}
	switch next.Observation.Status {
	case "succeeded", "failed", "denied", "cancelled", "timed_out", "outcome_unknown":
	default:
		return product.NewError(product.CodeInvalidArgument, "invalid observation status")
	}
	switch next.Observation.SideEffect {
	case "none", "confirmed", "unknown":
	default:
		return product.NewError(product.CodeInvalidArgument, "invalid side effect")
	}
	if v.Observations == nil {
		v.Observations = map[string]ObservationRevision{}
	}
	v.Observations[next.ID] = next
	return nil
}

// CommitReconciliation appends the next observation, reconciliation result,
// optional trusted no-start resource release and operation completion in one
// journal commit. It never rewrites the original ToolRecord or refunds its
// claim/budget.
func (m *Manager) CommitReconciliation(ctx context.Context, operationID string, expectedRevision uint64, next ObservationRevision, result Reconciliation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.view.Operations[operationID]
	if !ok || op.Kind != "reconcile" || op.State != "running" || op.Revision != expectedRevision || result.OperationID != operationID || result.ID == "" || result.CallID == "" || result.NewObservationID != next.ID || result.ObservationID != next.PreviousID || result.ObservationVersion != next.Version-1 {
		return product.NewError(product.CodeStateConflict, "reconciliation operation or evidence is stale")
	}
	old, ok := m.view.latestObservation(next.CallID)
	if !ok || old.ID != result.ObservationID || old.Version != result.ObservationVersion || next.Version != old.Version+1 || next.PreviousID != old.ID {
		return product.NewError(product.CodeStateConflict, "reconciliation observation revision changed")
	}
	if next.ID == "" || next.CallID != result.CallID || result.EvidenceSource == "" || len(result.EvidenceRefs) == 0 {
		return product.NewError(product.CodeInvalidArgument, "reconciliation evidence is incomplete")
	}
	nextOperation := op
	nextOperation.Revision++
	nextOperation.State = "completed"
	nextOperation.ResultRef = result.ID
	controls := []store.Record{
		record("observation_revision", next.ID, next),
		record("reconciliation", result.ID, result),
		record("operation", operationID, nextOperation),
	}
	call := m.view.Calls[next.CallID]
	if result.TrustedNoStart {
		trace := m.view.Traces[call.Scope.TraceID]
		if trace == nil {
			return product.NewError(product.CodeStateConflict, "release requires stopped execution proof")
		}
		release := ResourceHoldRelease{ID: "release:" + result.ID, CallID: next.CallID, ReconciliationID: result.ID, ObservationID: next.ID, ExecutionID: trace.ExecutionID}
		controls = append(controls, record("resource_hold_release", release.ID, release))
	}
	payload := struct {
		CallID           string `json:"callId"`
		ObservationID    string `json:"observationId"`
		ReconciliationID string `json:"reconciliationId"`
		Status           string `json:"status"`
		SideEffect       string `json:"sideEffect"`
		Executed         bool   `json:"executed"`
	}{next.CallID, next.ID, result.ID, next.Observation.Status, next.Observation.SideEffect, next.Observation.Executed}
	_, err := m.commit(ctx, controls, nil, []agent.Event{m.event("tool.state_changed", call.Scope.TraceID, call.Scope.TurnID, payload)})
	return err
}

// identity is only a replay reference; version 1 is never re-written to the log.
func applyLegacyObservation(v *View, call agent.ToolRecord) error {
	if call.Observation == nil {
		return nil
	}
	id := "legacy:" + call.Call.CallID
	initial := ObservationRevision{ID: id, CallID: call.Call.CallID, Version: 1, Observation: *call.Observation}
	if existing, ok := v.Observations[id]; ok {
		if existing.Observation != initial.Observation {
			return product.NewError(product.CodeStateConflict, "legacy observation is immutable")
		}
		return nil
	}
	if latest, ok := v.latestObservation(call.Call.CallID); ok {
		if latest.Version == 1 && latest.Observation == initial.Observation {
			return nil
		}
		return product.NewError(product.CodeStateConflict, "legacy result cannot overwrite versioned evidence")
	}
	return putImmutable(&v.Observations, id, id, initial)
}
func applyReconciliation(v *View, r store.Record) error {
	var value Reconciliation
	if err := json.Unmarshal(r.Payload, &value); err != nil {
		return err
	}
	op, ok := v.Operations[value.OperationID]
	if !ok || op.Kind != "reconcile" || op.Receipt.Target != value.CallID || (op.State != "accepted" && op.State != "running") {
		return product.NewError(product.CodeStateConflict, "reconciliation operation is not active")
	}
	previous, ok := v.Observations[value.ObservationID]
	next, nextOK := v.Observations[value.NewObservationID]
	if !ok || !nextOK || previous.CallID != value.CallID || next.CallID != value.CallID || previous.Version != value.ObservationVersion || next.PreviousID != previous.ID {
		return product.NewError(product.CodeStateConflict, "reconciliation observation mismatch")
	}
	return putImmutable(&v.Reconciliations, r.ID, value.ID, value)
}
