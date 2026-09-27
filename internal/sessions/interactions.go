package sessions

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

type InteractionResponse struct {
	InteractionID    string          `json:"interactionId"`
	Decision         string          `json:"decision,omitempty"`
	Answer           json.RawMessage `json:"answer,omitempty"`
	ExpectedRevision uint64          `json:"expectedRevision"`
	IdempotencyKey   string          `json:"idempotencyKey,omitempty"`
}

func (rt *runtime) approvalNow() time.Time {
	if rt.clock != nil {
		return rt.clock.Now()
	}
	return time.Now()
}

func (rt *runtime) RequestToolApproval(ctx context.Context, scope agent.ExecutionScope, frozen agent.FrozenExecution) (*agent.ApprovalWait, error) {
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		decision, err := rt.checkToolPolicy(ctx, scope, frozen)
		if err != nil {
			return nil, err
		}
		fingerprint := resumeBuildFingerprint(rt.opts)
		if frozen.Origin == "direct" {
			fingerprint = directBuildFingerprint(rt.opts)
		}
		if decision != agent.DecisionAsk || rt.opts.Principal == "" || fingerprint == "" {
			return nil, product.NewError(product.CodePermissionDenied, "execution is not awaiting a supported approval")
		}
		in, err := rt.manager.RequestApproval(ctx, rt.manager.View().LastSeq, frozen.ID, "Approve this operation once?", rt.approvalNow())
		if err != nil {
			return nil, err
		}
		return &agent.ApprovalWait{InteractionID: in.ID}, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*agent.ApprovalWait), nil
}

func (rt *runtime) approvalWaitPresent(frame *execution) (bool, error) {
	value, err := rt.call(context.Background(), func(rt *runtime) (any, error) {
		v := rt.manager.View()
		scope := frame.scope
		scope.TurnID = frame.turnID
		for _, in := range v.Interactions {
			call := v.Calls[in.CallID]
			if in.Kind == "approval" && call.Observation == nil && !call.Claimed && rt.matchesCallScope(scope, call) {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return false, err
	}
	return value.(bool), nil
}

func (rt *runtime) approvalTargets(frame *execution, contexts []*adk.InterruptCtx) (map[string]string, error) {
	value, err := rt.call(context.Background(), func(rt *runtime) (any, error) {
		if rt.active != frame || frame.ctx.Err() != nil {
			return nil, product.NewError(product.CodeStateConflict, "approval execution is no longer active")
		}
		v := rt.manager.View()
		targets := make(map[string]string)
		for _, interrupted := range contexts {
			if interrupted == nil || !interrupted.IsRootCause {
				continue
			}
			id, isInteraction := interrupted.Info.(string)
			in, exists := v.Interactions[id]
			call := v.Calls[in.CallID]
			matchedAddress := false
			for _, segment := range interrupted.Address {
				if segment.Type == adk.AddressSegmentTool && segment.ID == call.Call.Name && segment.SubID == call.Call.ProviderCallID {
					matchedAddress = true
				}
			}
			scope := frame.scope
			scope.TurnID = frame.turnID
			if !isInteraction || !exists || in.Kind != "approval" || in.Scope != call.Scope || !rt.matchesCallScope(scope, call) || call.Claimed || call.Observation != nil || interrupted.ID == "" || targets[id] != "" || !matchedAddress {
				return nil, incompatibleResume("framework interrupt is not an original approval target")
			}
			targets[id] = interrupted.ID
		}
		if len(targets) == 0 {
			return nil, incompatibleResume("approval checkpoint has no trusted interrupt targets")
		}
		return targets, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(map[string]string), nil
}

func (rt *runtime) validateApprovalResume(cp state.CheckpointRef, v state.View) error {
	for _, id := range cp.InteractionIDs {
		in, exists := v.Interactions[id]
		approval := v.Approvals[in.ApprovalID]
		binding, bound := v.ApprovalBindings[cp.ID+":"+id]
		if !exists || !bound || in.Kind != "approval" || in.ApprovalID != approval.ID || approval.InteractionID != id || in.Scope != approval.Scope || !sameLogicalScope(in.Scope, cp.Scope) || binding.ApprovalID != approval.ID || binding.CheckpointID != cp.ID || binding.TargetRef == "" {
			return incompatibleResume("approval is not associated with the original checkpoint")
		}
		now := rt.approvalNow()
		if !now.Before(approval.ExpiresAt) || now.Before(approval.ExpiresAt.Add(-config.ApprovalValidity)) {
			return product.NewError(product.CodePermissionDenied, "checkpoint approval is expired")
		}
		if answer, answered := v.ApprovalDecisions[approval.ID]; answered {
			op := v.Operations[answer.OperationID]
			if answer.BindingID != binding.ID || answer.InteractionID != id || answer.Principal == "" || answer.Principal != op.Principal || op.SessionID != cp.Scope.SessionID || op.Receipt.Target != id || (answer.Decision != "allowed-once" && answer.Decision != "rejected" && answer.Decision != "cancelled") {
				return incompatibleResume("approval decision is not a saved answer for this checkpoint")
			}
		}
	}
	return nil
}

func approvalResumeTargets(cp state.CheckpointRef, v state.View) map[string]any {
	targets := make(map[string]any)
	for _, id := range cp.InteractionIDs {
		in := v.Interactions[id]
		binding := v.ApprovalBindings[cp.ID+":"+id]
		if answer, answered := v.ApprovalDecisions[in.ApprovalID]; answered {
			targets[binding.TargetRef] = answer.Decision
		}
	}
	return targets
}

func (rt *runtime) toolApprovalDecision(v state.View, frozen agent.FrozenExecution, claimed bool) (agent.Decision, error) {
	for _, approval := range v.Approvals {
		if approval.FrozenExecutionID != frozen.ID || approval.FrozenHash != frozen.Hash || approval.Scope != frozen.Scope {
			continue
		}
		if !rt.approvalNow().Before(approval.ExpiresAt) {
			return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "execution approval is expired")
		}
		answer, answered := v.ApprovalDecisions[approval.ID]
		if !answered {
			return agent.DecisionAsk, nil
		}
		if answer.Decision != "allowed-once" {
			return agent.DecisionDeny, nil
		}
		binding := v.ApprovalBindings[answer.BindingID]
		consumption, consumed := v.ApprovalClaims[approval.ID]
		bound := rt.active.resume != nil && binding.CheckpointID == rt.active.resume.ID && binding.DirectResumeID == ""
		if frozen.Origin == "direct" {
			bound = rt.active.directResume != nil && binding.DirectResumeID == rt.active.directResume.ID && binding.CheckpointID == "" && rt.active.directResume.CallID == frozen.CallID && rt.active.directResume.FrozenHash == frozen.Hash
		}
		if !bound || claimed != consumed || (consumed && (consumption.ExecutionID != rt.active.scope.ExecutionID || consumption.CallID != frozen.CallID)) {
			return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "approval is not for the current resumed execution")
		}
		return agent.DecisionAllow, nil
	}
	if frozen.Origin == "direct" && rt.opts.Principal != "" && directBuildFingerprint(rt.opts) != "" {
		return agent.DecisionAsk, nil
	}
	if _, hasBlobs := rt.opts.Store.(store.CheckpointBlobs); hasBlobs && rt.opts.Principal != "" && resumeBuildFingerprint(rt.opts) != "" {
		return agent.DecisionAsk, nil
	}
	return agent.DecisionAsk, product.NewError(product.CodeResourceUnavailable, "execution approval is not available")
}

func (s *AgentSession) RespondInteraction(ctx context.Context, response InteractionResponse) (state.OperationReceipt, error) {
	if len(response.Answer) != 0 {
		if response.Decision != "" {
			return state.OperationReceipt{}, product.NewError(product.CodeInvalidArgument, "interaction answer and approval decision are mutually exclusive")
		}
		return state.OperationReceipt{}, product.NewError(product.CodeUnsupportedCapability, "workflow answers are not available")
	}
	content, err := json.Marshal(struct {
		Decision string `json:"decision"`
	}{response.Decision})
	if err != nil {
		return state.OperationReceipt{}, err
	}
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		cmd := state.OperationCommand{Principal: rt.opts.Principal, Kind: "respond_interaction", Target: response.InteractionID, ExpectedRevision: response.ExpectedRevision, IdempotencyKey: response.IdempotencyKey, Content: content}
		receipt, err := rt.manager.RespondApproval(ctx, cmd, response.Decision, rt.approvalNow())
		if err != nil {
			return nil, err
		}
		status, err := rt.manager.GetOperation(receipt.OperationID)
		if err != nil {
			return nil, err
		}
		if status.State == "accepted" {
			if err := rt.manager.TransitionOperation(context.WithoutCancel(ctx), receipt.OperationID, status.Revision, "completed", response.InteractionID, ""); err != nil {
				return nil, err
			}
		}
		return receipt, nil
	})
	if err != nil {
		return state.OperationReceipt{}, err
	}
	return value.(state.OperationReceipt), nil
}

func (rt *runtime) snapshotApprovals(v state.View) (map[string]state.Interaction, map[string]state.Approval) {
	interactions := make(map[string]state.Interaction, len(v.Interactions))
	approvals := make(map[string]state.Approval, len(v.Approvals))
	for id, in := range v.Interactions {
		approval := v.Approvals[in.ApprovalID]
		tr := v.Traces[in.Scope.TraceID]
		in.TargetRef = "" // Framework targets are never public response inputs.
		if tr != nil {
			if binding, bound := v.ApprovalBindings[tr.DirectResumeID+":"+id]; bound && binding.DirectResumeID != "" {
				in.State = "ready"
			}
			if binding, bound := v.ApprovalBindings[tr.CheckpointID+":"+id]; bound {
				in.State, in.CheckpointRef = "ready", binding.CheckpointID
			}
		}
		if answer, answered := v.ApprovalDecisions[approval.ID]; answered {
			in.State, approval.State, approval.Principal = answer.Decision, answer.Decision, answer.Principal
			in.CheckpointRef = v.ApprovalBindings[answer.BindingID].CheckpointID
		}
		_, claimed := v.ApprovalClaims[approval.ID]
		if claimed {
			in.State, approval.State = "claimed", "claimed"
		} else if (in.State == "pending" || in.State == "ready" || in.State == "allowed-once") && !rt.approvalNow().Before(in.ExpiresAt) {
			in.State, approval.State = "expired", "expired"
		}
		if tr != nil && tr.State == "cancelled" && !claimed {
			in.State, approval.State = "cancelled", "cancelled"
		}
		interactions[id], approvals[approval.ID] = in, approval
	}
	return interactions, approvals
}

func approvalIDForCall(v state.View, frozen agent.FrozenExecution) string {
	for id, approval := range v.Approvals {
		if approval.FrozenExecutionID == frozen.ID && approval.FrozenHash == frozen.Hash && approval.Scope == frozen.Scope {
			return id
		}
	}
	return ""
}
