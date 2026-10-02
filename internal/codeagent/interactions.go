package codeagent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
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
		if frozen.Origin == "direct" {
			return nil, product.NewError(product.CodePermissionDenied, "direct command approvals are no longer supported")
		}
		if frozen.Origin == "workflow_node" {
			return nil, product.NewError(product.CodePermissionDenied, "embedded workflow approvals are no longer supported")
		}
		if decision != agent.DecisionAsk || rt.opts.Principal == "" || resumeBuildFingerprint(rt.opts) == "" {
			return nil, product.NewError(product.CodePermissionDenied, "execution is not awaiting a supported approval")
		}
		pending := rt.newApproval(frozen)
		return &agent.ApprovalWait{InteractionID: pending.interaction.ID}, nil
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
		for _, pending := range rt.approvals {
			call := v.Calls[pending.interaction.CallID]
			if call.Observation == nil && !call.Claimed && rt.matchesCallScope(scope, call) {
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
			pending := rt.approvals[id]
			// Eino may retain a legacy or previous-instance interrupt payload. Match
			// only its saved target and original call; the payload cannot grant access.
			if pending == nil && frame.resume != nil {
				for _, candidate := range rt.approvals {
					if candidate.checkpointID != frame.resume.ID {
						continue
					}
					call := v.Calls[candidate.interaction.CallID]
					for _, segment := range interrupted.Address {
						if segment.Type == adk.AddressSegmentTool && segment.ID == call.Call.Name && segment.SubID == call.Call.ProviderCallID {
							pending = candidate
							break
						}
					}
					if pending != nil {
						break
					}
				}
			}
			if !isInteraction || pending == nil {
				return nil, incompatibleResume("framework interrupt is not an original approval target")
			}
			in := pending.interaction
			call := v.Calls[in.CallID]
			matchedAddress := false
			for _, segment := range interrupted.Address {
				if segment.Type == adk.AddressSegmentTool && segment.ID == call.Call.Name && segment.SubID == call.Call.ProviderCallID {
					matchedAddress = true
				}
			}
			scope := frame.scope
			scope.TurnID = frame.turnID
			if in.Scope != call.Scope || !rt.matchesCallScope(scope, call) || call.Claimed || call.Observation != nil || interrupted.ID == "" || targets[in.CallID] != "" || !matchedAddress {
				return nil, incompatibleResume("framework interrupt is not an original approval target")
			}
			targets[in.CallID] = interrupted.ID
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
	targets := checkpointApprovalTargets(cp, v)
	for callID, target := range targets {
		call, exists := v.Calls[callID]
		frozen, saved := v.FrozenExecutions["execution:"+callID]
		if !exists || !saved || target == "" || frozen.Scope != call.Scope || !sameLogicalScope(call.Scope, cp.Scope) || frozen.RequestedGrantRef == "" {
			return incompatibleResume("approval recovery target does not match original frozen call")
		}
		if call.Observation != nil || call.Claimed {
			continue
		}
		for _, pending := range rt.approvals {
			if pending.approval.CallID == callID && pending.checkpointID == cp.ID && !approvalWithinValidity(rt.approvalNow(), pending.approval) {
				return product.NewError(product.CodePermissionDenied, "checkpoint approval is expired")
			}
		}
	}
	return nil
}

func (rt *runtime) approvalResumeTargets(cp state.CheckpointRef) map[string]any {
	targets := make(map[string]any)
	for _, pending := range rt.approvals {
		if pending.checkpointID == cp.ID && pending.decision != "" {
			targets[pending.target] = pending.decision
		}
	}
	return targets
}

func (rt *runtime) toolApprovalDecision(v state.View, frozen agent.FrozenExecution, claimed bool) (agent.Decision, error) {
	for _, pending := range rt.approvals {
		a := pending.approval
		if a.FrozenExecutionID != frozen.ID || a.FrozenHash != frozen.Hash || a.Scope != frozen.Scope {
			continue
		}
		if !approvalWithinValidity(rt.approvalNow(), a) {
			return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "execution approval is expired")
		}
		if pending.decision == "" {
			return agent.DecisionAsk, nil
		}
		if pending.decision != "allowed-once" {
			return agent.DecisionDeny, nil
		}
		consumed := pending.claimedExecution != ""
		if rt.active == nil || rt.active.resume == nil || pending.checkpointID != rt.active.resume.ID || claimed != consumed || (consumed && pending.claimedExecution != rt.active.scope.ExecutionID) {
			return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "approval is not for the current resumed execution")
		}
		return agent.DecisionAllow, nil
	}
	if frozen.Origin == "workflow_node" {
		return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "embedded workflow approvals are no longer supported")
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
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		return rt.answerApproval(response)
	})
	if err != nil {
		return state.OperationReceipt{}, err
	}
	return value.(state.OperationReceipt), nil
}

func (rt *runtime) snapshotApprovals(v state.View) (map[string]state.Interaction, map[string]state.Approval) {
	interactions := make(map[string]state.Interaction, len(rt.approvals))
	approvals := make(map[string]state.Approval, len(rt.approvals))
	for id, pending := range rt.approvals {
		in, approval := pending.interaction, pending.approval
		in.Options = append([]string(nil), in.Options...)
		tr := v.Traces[in.Scope.TraceID]
		if pending.checkpointID != "" {
			in.State, in.CheckpointRef = "ready", pending.checkpointID
		}
		if pending.decision != "" {
			in.State, approval.State = pending.decision, pending.decision
		}
		if pending.claimedExecution != "" {
			in.State, approval.State = "claimed", "claimed"
		} else if !approvalWithinValidity(rt.approvalNow(), approval) {
			in.State, approval.State = "expired", "expired"
		}
		if tr != nil && tr.State == "cancelled" && pending.claimedExecution == "" {
			in.State, approval.State = "cancelled", "cancelled"
		}
		// UTC is a presentation detail; retain monotonic deadlines in runtime.
		in.ExpiresAt, approval.ExpiresAt = in.ExpiresAt.UTC(), approval.ExpiresAt.UTC()
		interactions[id], approvals[approval.ID] = in, approval
	}
	return interactions, approvals
}
