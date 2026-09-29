package sessions

import (
	"fmt"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func TestApprovalLegacyDecisionIsRecoveryMetadataOnly(t *testing.T) {
	f := waitingApprovalSession(t, nil)
	if err := f.s.rt.do(t.Context(), func(rt *runtime) error {
		v := rt.manager.View()
		cp := v.Checkpoints[v.Traces[f.input.TraceID].CheckpointID]
		var callID, target string
		for call, address := range cp.ApprovalTargets {
			callID, target = call, address
		}
		frozen := v.FrozenExecutions["execution:"+callID]
		old := state.Approval{ID: "legacy-approval", InteractionID: "legacy-interaction", CallID: callID, Scope: frozen.Scope, FrozenExecutionID: frozen.ID, FrozenHash: frozen.Hash, GrantRef: frozen.RequestedGrantRef, State: "asked", ExpiresAt: rt.approvalNow().Add(-time.Hour)}
		v.Approvals[old.ID] = old
		v.Interactions[old.InteractionID] = state.Interaction{ID: old.InteractionID, Kind: "approval", Scope: old.Scope, CallID: callID, ApprovalID: old.ID}
		binding := state.ApprovalBinding{ID: cp.ID + ":" + old.InteractionID, InteractionID: old.InteractionID, ApprovalID: old.ID, CheckpointID: cp.ID, TargetRef: target}
		v.ApprovalBindings[binding.ID] = binding
		v.ApprovalDecisions[old.ID] = state.ApprovalDecision{ApprovalID: old.ID, InteractionID: old.InteractionID, BindingID: binding.ID, Decision: "allowed-once", Principal: "old-user"}
		cp.ApprovalTargets, cp.InteractionIDs = nil, []string{old.InteractionID}
		v.Checkpoints[cp.ID] = cp
		rt.approvals = nil
		rt.active = &execution{scope: cp.Scope, ctx: t.Context(), resume: &cp}
		defer func() { rt.active = nil }()
		if err := rt.prepareApprovalResume(t.Context(), cp, v); err != nil {
			return err
		}
		if len(rt.approvals) != 1 || rt.approvals[old.InteractionID] != nil {
			return fmt.Errorf("legacy response identity was recovered")
		}
		decision, err := rt.toolApprovalDecision(v, frozen, false)
		if err != nil || decision != agent.DecisionAsk || len(rt.approvalResumeTargets(cp)) != 0 {
			return fmt.Errorf("historical decision restored permission: decision=%v err=%v", decision, err)
		}
		if err := rt.validateApprovalResume(cp, v); err != nil {
			return fmt.Errorf("legacy expired decision blocked fresh request: %w", err)
		}
		for _, pending := range rt.approvals {
			if pending.approval.ID == old.ID || pending.decision != "" || pending.claimedExecution != "" || pending.checkpointID != cp.ID || pending.target != target {
				return fmt.Errorf("legacy binding changed runtime permission")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if f.runs.Load() != 0 || f.model.Calls() != 1 || f.manager.View().Traces[f.input.TraceID].Usage.ToolExecutions != 0 {
		t.Fatal("legacy recovery executed work")
	}
}
