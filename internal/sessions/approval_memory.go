package sessions

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

// runtimeApproval belongs exclusively to the session mailbox. Checkpoint targets
// describe where to resume; they never carry a decision or a consumed permission.
type runtimeApproval struct {
	interaction      state.Interaction
	approval         state.Approval
	checkpointID     string
	target           string
	decision         string
	claimedExecution string
}

// commandBlockedByApproval runs in the mailbox before registering a host shell.
// An answer does not release a paused checkpoint. Persisted targets also protect
// reopened sessions, whose process-local approval questions are intentionally gone.
func (rt *runtime) commandBlockedByApproval() bool {
	v := rt.manager.View()
	tr := v.Traces[v.ActiveTrace]
	if tr == nil || terminal(tr.State) {
		return false
	}
	if tr.State == "paused" {
		// Old direct commands retain their approval association on disk, while
		// process-local questions intentionally disappear on reopen. An answer
		// does not settle this stopped Trace; only cancellation releases it.
		if b, ok := v.DirectResumes[tr.DirectResumeID]; ok && b.Scope.TraceID == tr.ID && v.ValidateDirectBinding(b) == nil {
			binding := v.ApprovalBindings[b.ID+":"+b.InteractionID]
			call := v.Calls[b.CallID]
			if binding.DirectResumeID == b.ID && binding.ApprovalID == b.ApprovalID && binding.InteractionID == b.InteractionID && !call.Claimed && call.Observation == nil {
				return true
			}
		}
		cp := v.Checkpoints[tr.CheckpointID]
		if len(cp.ApprovalTargets) != 0 || len(cp.InteractionIDs) != 0 {
			return true
		}
	}
	for _, pending := range rt.approvals {
		if pending.approval.Scope.TraceID != tr.ID {
			continue
		}
		call, ok := v.Calls[pending.approval.CallID]
		if ok && !call.Claimed && call.Observation == nil {
			return true
		}
	}
	return false
}

func (rt *runtime) newApproval(frozen agent.FrozenExecution) *runtimeApproval {
	if rt.approvals == nil {
		rt.approvals = make(map[string]*runtimeApproval)
	}
	for _, pending := range rt.approvals {
		if pending.approval.FrozenExecutionID == frozen.ID && pending.approval.FrozenHash == frozen.Hash && pending.approval.Scope == frozen.Scope {
			return pending
		}
	}
	// Permissions live only in this process. Keep the monotonic reading so
	// system wall-clock corrections cannot shorten or extend their 24h lifetime.
	expiry := rt.approvalNow().Add(config.ApprovalValidity)
	a := state.Approval{ID: agent.MustID(), InteractionID: agent.MustID(), CallID: frozen.CallID, Scope: frozen.Scope, FrozenExecutionID: frozen.ID, FrozenHash: frozen.Hash, GrantRef: frozen.RequestedGrantRef, State: "asked", ExpiresAt: expiry}
	pending := &runtimeApproval{approval: a, interaction: state.Interaction{ID: a.InteractionID, Kind: "approval", Scope: a.Scope, CallID: a.CallID, ApprovalID: a.ID, Question: "Approve this operation once?", Options: []string{"allowed-once", "rejected", "cancelled"}, State: "pending", ExpiresAt: expiry}}
	rt.approvals[a.InteractionID] = pending
	return pending
}

// checkpointApprovalTargets also decodes legacy bindings as recovery locations,
// without importing legacy approval IDs, decisions, receipts or expiry times.
func checkpointApprovalTargets(cp state.CheckpointRef, v state.View) map[string]string {
	targets := make(map[string]string, len(cp.ApprovalTargets))
	for callID, target := range cp.ApprovalTargets {
		targets[callID] = target
	}
	for _, id := range cp.InteractionIDs {
		in := v.Interactions[id]
		binding := v.ApprovalBindings[cp.ID+":"+id]
		targets[in.CallID] = binding.TargetRef
	}
	return targets
}

// prepareApprovalResume runs only in the original GenResume callback after
// Resume acceptance. Checkpoint targets locate calls, but do not grant permission
// or create questions during Open or read-only browsing.
func (rt *runtime) prepareApprovalResume(ctx context.Context, cp state.CheckpointRef, v state.View) error {
	for callID, target := range checkpointApprovalTargets(cp, v) {
		call := v.Calls[callID]
		if call.Claimed || call.Observation != nil {
			continue
		}
		frozen := v.FrozenExecutions["execution:"+callID]
		present := false
		for _, pending := range rt.approvals {
			if pending.approval.FrozenExecutionID == frozen.ID && pending.approval.FrozenHash == frozen.Hash && pending.approval.Scope == frozen.Scope && pending.checkpointID == cp.ID {
				present = true
				break
			}
		}
		if present {
			continue // The original tool path revalidates an existing answer.
		}
		decision, err := rt.checkToolPolicy(ctx, call.Scope, frozen)
		if err != nil {
			return err
		}
		if decision == agent.DecisionAsk {
			pending := rt.newApproval(frozen)
			pending.checkpointID, pending.target = cp.ID, target
		}
	}
	return nil
}

// bindApprovalCheckpoint makes already requested runtime questions answerable
// once the execution has safely stopped. It never creates a new permission.
func (rt *runtime) bindApprovalCheckpoint(cp state.CheckpointRef) {
	for _, pending := range rt.approvals {
		if target := cp.ApprovalTargets[pending.approval.CallID]; target != "" {
			pending.checkpointID, pending.target = cp.ID, target
		}
	}
}

func (rt *runtime) answerApproval(response InteractionResponse) (state.OperationReceipt, error) {
	if rt.opts.Principal == "" {
		return state.OperationReceipt{}, product.NewError(product.CodePermissionDenied, "approval response requires a trusted principal")
	}
	if response.Decision != "allowed-once" && response.Decision != "rejected" && response.Decision != "cancelled" {
		return state.OperationReceipt{}, product.NewError(product.CodeInvalidArgument, "invalid approval decision")
	}
	raw, _ := json.Marshal(response)
	digest := string(raw)
	for _, op := range rt.approvalOps {
		if response.IdempotencyKey != "" && op.Key == response.IdempotencyKey && op.Principal == rt.opts.Principal {
			if op.Digest != digest {
				return state.OperationReceipt{}, product.NewError(product.CodeIdempotencyConflict, "key already belongs to a different approval response")
			}
			return op.Receipt, nil
		}
	}
	v := rt.manager.View()
	if response.ExpectedRevision != v.LastSeq {
		return state.OperationReceipt{}, product.NewError(product.CodeStateConflict, "session revision changed")
	}
	pending := rt.approvals[response.InteractionID]
	if pending == nil {
		return state.OperationReceipt{}, product.NewError(product.CodeNotFound, "interaction not found")
	}
	a := pending.approval
	tr := v.Traces[a.Scope.TraceID]
	call := v.Calls[a.CallID]
	frozen := v.FrozenExecutions[a.FrozenExecutionID]
	if pending.decision != "" || pending.checkpointID == "" || pending.target == "" || tr == nil || tr.State != "paused" || !tr.ExecutionStopped || tr.Settled || tr.CheckpointID != pending.checkpointID || call.Claimed || call.Observation != nil {
		return state.OperationReceipt{}, product.NewError(product.CodeStateConflict, "interaction has no pending stopped checkpoint")
	}
	now := rt.approvalNow()
	if now.IsZero() || !now.Before(a.ExpiresAt) || now.Before(a.ExpiresAt.Add(-config.ApprovalValidity)) {
		return state.OperationReceipt{}, product.NewError(product.CodePermissionDenied, "approval is expired")
	}
	if frozen.Hash != a.FrozenHash || frozen.Scope != a.Scope || frozen.PolicyRef != v.ExecutionPolicy.Ref || v.ExecutionPolicy.ApprovalPolicy == "never" || (v.ExecutionPolicy.SandboxMode == "read-only" && frozen.Effect != "read" && frozen.Effect != "none") {
		return state.OperationReceipt{}, product.NewError(product.CodePermissionDenied, "current policy denies approval")
	}
	receipt := state.OperationReceipt{OperationID: agent.MustID(), State: "accepted", Target: a.InteractionID}
	if rt.approvalOps == nil {
		rt.approvalOps = make(map[string]state.Operation)
	}
	rt.approvalOps[receipt.OperationID] = state.Operation{Receipt: receipt, Principal: rt.opts.Principal, SessionID: rt.opts.SessionID, Kind: "respond_interaction", Key: response.IdempotencyKey, Digest: digest, Revision: 1, State: "completed", ResultRef: a.InteractionID}
	pending.decision, pending.approval.Principal = response.Decision, rt.opts.Principal
	return receipt, nil
}

func (rt *runtime) claimRuntimeApproval(ctx context.Context, frozen agent.FrozenExecution, call agent.FrozenCall, usage agent.Usage) error {
	for _, pending := range rt.approvals {
		if pending.approval.FrozenExecutionID != frozen.ID || pending.approval.FrozenHash != frozen.Hash || pending.approval.Scope != frozen.Scope {
			continue
		}
		if pending.decision != "allowed-once" || pending.claimedExecution != "" || rt.active == nil || rt.active.resume == nil || pending.checkpointID != rt.active.resume.ID {
			return product.NewError(product.CodePermissionDenied, "no unconsumed runtime approval")
		}
		if err := rt.manager.ClaimRuntimeApprovedTool(ctx, call, usage, rt.active.scope.ExecutionID); err != nil {
			return err
		}
		pending.claimedExecution = rt.active.scope.ExecutionID
		return nil
	}
	return product.NewError(product.CodePermissionDenied, "runtime approval unavailable")
}

func approvalWithinValidity(now time.Time, a state.Approval) bool {
	return !now.IsZero() && now.Before(a.ExpiresAt) && !now.Before(a.ExpiresAt.Add(-config.ApprovalValidity))
}
