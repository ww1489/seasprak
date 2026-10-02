package state

import (
	"encoding/json"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// ResourceHoldRelease is an immutable scheduling release, not a claim or budget
// refund. The referenced reconciliation and observation are committed with it.
type ResourceHoldRelease struct {
	ID               string `json:"id"`
	CallID           string `json:"callId"`
	ReconciliationID string `json:"reconciliationId"`
	ObservationID    string `json:"observationId"`
	ExecutionID      string `json:"executionId,omitempty"`
}

func applyResourceHoldRelease(v *View, r store.Record) error {
	var release ResourceHoldRelease
	if err := json.Unmarshal(r.Payload, &release); err != nil {
		return err
	}
	result, ok := v.Reconciliations[release.ReconciliationID]
	observation, observed := v.latestObservation(release.CallID)
	call, claimed := v.Calls[release.CallID]
	trace := v.Traces[call.Scope.TraceID]
	op := v.Operations[result.OperationID]
	if !ok || !observed || !claimed || !call.Claimed || trace == nil || !trace.ExecutionStopped || trace.InvocationID != call.Scope.InvocationID || trace.ExecutionID != release.ExecutionID ||
		!result.TrustedNoStart || result.QueryID == "" || result.EvidenceSource == "" || len(result.EvidenceRefs) == 0 || len(result.ConfirmedEffects) != 0 || len(result.RemainingUnknown) != 0 || len(result.ConflictRestrictions) != 0 ||
		result.CallID != release.CallID || result.NewObservationID != release.ObservationID || observation.ID != release.ObservationID || observation.Observation.Executed || observation.Observation.SideEffect != "none" || observation.Observation.Status != "cancelled" ||
		op.State != "completed" || op.ResultRef != result.ID {
		return product.NewError(product.CodeStateConflict, "resource release lacks trusted no-start and stopped execution proof")
	}
	return putImmutable(&v.ResourceHoldReleases, r.ID, release.ID, release)
}

// ResourceHoldReleased uses the committed release only while its reconciliation
// is still effective. Late conflicting evidence conservatively restores the hold.
func (v View) ResourceHoldReleased(callID string) bool {
	if v.ReconciliationUnresolved(callID) {
		return false
	}
	observation, ok := v.EffectiveObservation(callID)
	if !ok {
		return false
	}
	for _, release := range v.ResourceHoldReleases {
		if release.CallID == callID && release.ObservationID == observation.ID {
			return true
		}
	}
	return false
}

// Validate the transaction boundary on replay as well as before Append. A
// release cannot be separated from its new observation and operation completion.
func validateResourceHoldReleaseCommit(c store.Commit) error {
	records := map[string]map[string]store.Record{}
	for _, r := range c.ControlRecords {
		if records[r.Type] == nil {
			records[r.Type] = map[string]store.Record{}
		}
		records[r.Type][r.ID] = r
	}
	for _, r := range c.ControlRecords {
		switch r.Type {
		case "reconciliation":
			var result Reconciliation
			if err := json.Unmarshal(r.Payload, &result); err != nil {
				return err
			}
			if result.TrustedNoStart {
				if _, ok := records["resource_hold_release"]["release:"+result.ID]; !ok {
					return product.NewError(product.CodeStateConflict, "trusted no-start requires an atomic resource release")
				}
			}
		case "resource_hold_release":
			var release ResourceHoldRelease
			if err := json.Unmarshal(r.Payload, &release); err != nil {
				return err
			}
			reconciliation, ok := records["reconciliation"][release.ReconciliationID]
			if !ok || r.ID != "release:"+release.ReconciliationID {
				return product.NewError(product.CodeStateConflict, "resource release requires its reconciliation in the same commit")
			}
			var result Reconciliation
			if err := json.Unmarshal(reconciliation.Payload, &result); err != nil {
				return err
			}
			_, observed := records["observation_revision"][release.ObservationID]
			_, completed := records["operation"][result.OperationID]
			if !observed || !completed {
				return product.NewError(product.CodeStateConflict, "resource release requires observation and operation in the same commit")
			}
		}
	}
	return nil
}
