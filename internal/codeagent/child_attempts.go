package codeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
)

// startModelAttempt commits the child's original identity before the model can
// reserve or send a request. A child never borrows its parent's model Turn.
func (s *childSink) startModelAttempt(ctx context.Context, scope agent.ExecutionScope, payload json.RawMessage) error {
	var identity agent.ModelAttemptIdentity
	if err := json.Unmarshal(payload, &identity); err != nil {
		return err
	}
	return s.rt.do(context.WithoutCancel(ctx), func(rt *runtime) error {
		if !rt.matchesDelegatedChild(scope) || scope.TurnID != "" || scope.SelectionRevision != 0 {
			return product.NewError(product.CodeStateConflict, "child model execution is not active")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rt.active.ctx.Err(); err != nil {
			return err
		}
		if identity.ID == "" || identity.ModelCallID == "" || identity.MessageID == "" || identity.StreamID == "" {
			return product.NewError(product.CodeStateConflict, "child model attempt identity is incomplete")
		}
		v := rt.manager.View()
		budget, reserved := v.InvocationBudgets[scope.InvocationID]
		_, originalCall := budget.Calls[identity.ModelCallID]
		if !reserved || budget.Scope != scope || !originalCall {
			return product.NewError(product.CodeStateConflict, "child attempt has no original logical model reservation")
		}
		switch identity.Purpose {
		case "agent", "compaction":
		case "workflow_node":
			return product.NewError(product.CodeIncompatibleVersion, "embedded workflow model attempts are no longer supported")
		default:
			return product.NewError(product.CodeStateConflict, "child model attempt purpose is invalid")
		}
		ordinal := uint64(1)
		for _, prior := range v.ModelAttempts {
			if prior.ModelCallID == identity.ModelCallID {
				if prior.Scope != scope {
					return product.NewError(product.CodeStateConflict, "child model call belongs to another execution")
				}
				ordinal++
			}
		}
		return rt.manager.SaveRecords(ctx, v.LastSeq, state.Records{ModelAttempts: []state.ModelAttempt{{
			ID: identity.ID, ModelCallID: identity.ModelCallID, MessageID: identity.MessageID, StreamID: identity.StreamID,
			Scope: scope, Purpose: identity.Purpose, Attempt: ordinal, State: "started", ModelConfigVersion: identity.ModelConfigVersion,
		}}})
	})
}

func (s *childSink) saveModelAttempt(ctx context.Context, scope agent.ExecutionScope, payload json.RawMessage) error {
	var body struct {
		AttemptID string                    `json:"attemptId"`
		Status    string                    `json:"status"`
		Message   *schema.AgenticMessage    `json:"message"`
		Details   agent.ModelAttemptDetails `json:"details"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return err
	}
	var accepted *agent.AgentMessage
	err := s.rt.do(context.WithoutCancel(ctx), func(rt *runtime) error {
		if !rt.matchesDelegatedChild(scope) || scope.TurnID != "" || scope.SelectionRevision != 0 {
			return product.NewError(product.CodeStateConflict, "child model execution is not active")
		}
		initial, ok := rt.manager.View().ModelAttempts[body.AttemptID]
		if !ok || initial.Scope != scope {
			return product.NewError(product.CodeStateConflict, "child model attempt was not registered")
		}
		status := body.Status
		var msg *agent.AgentMessage
		var calls []agent.ToolRecord
		if body.Message != nil {
			candidate := childAssistantMessage(scope, body.Message)
			candidate.ID = initial.MessageID
			candidate.Status = agent.StatusIncomplete
			msg = &candidate
		}
		if status == "complete" {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := rt.active.ctx.Err(); err != nil {
				return err
			}
			if msg == nil {
				return product.NewError(product.CodeInvalidArgument, "child assistant response is empty")
			}
			status, msg.Status = "accepted", agent.StatusComplete
			calls = rt.childToolRecords(scope, body.Message)
		}
		if err := rt.manager.SaveAttemptResult(context.WithoutCancel(ctx), scope, initial.ID, status, modelFinish(body.Message), msg, calls, body.Details); err != nil {
			return err
		}
		if status == "accepted" {
			accepted = msg
		}
		return nil
	})
	if err == nil && accepted != nil && s.compactor != nil {
		s.compactor.record(*accepted)
	}
	return err
}

// childToolRecords preserves the model's provider IDs and tool order. Calls
// become accepted only in the same transaction as the private candidate.
func (rt *runtime) childToolRecords(scope agent.ExecutionScope, msg *schema.AgenticMessage) []agent.ToolRecord {
	var calls []agent.ToolRecord
	for _, block := range msg.ContentBlocks {
		if block == nil || block.FunctionToolCall == nil {
			continue
		}
		fc := block.FunctionToolCall
		version := ""
		for _, def := range rt.opts.Tools {
			if def.Name == fc.Name {
				version = def.Version
				break
			}
		}
		sum := sha256.Sum256([]byte(fc.Name + "\n" + version + "\n" + fc.Arguments + "\n" + scope.Generation))
		calls = append(calls, agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: agent.MustID(), ProviderCallID: fc.CallID, Name: fc.Name, Arguments: fc.Arguments, Generation: scope.Generation, Hash: hex.EncodeToString(sum[:])}})
	}
	return calls
}
