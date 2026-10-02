package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// ValidateCodeCommitCompatibility rejects facts owned by the exited embedded
// workflow runtime without projecting state or performing any IO.
func ValidateCodeCommitCompatibility(c store.Commit) error {
	// Reject exited required facts before transaction validation or projection.
	// An old workflow must never become an ordinary P1 model/tool record.
	for _, r := range c.ControlRecords {
		if r.Type == "workflow_node" {
			return product.NewError(product.CodeIncompatibleVersion, "embedded workflow records are no longer supported")
		}
		if r.Type == "model_attempt" || r.Type == "frozen_execution" {
			var identity struct {
				Purpose string `json:"purpose"`
				Origin  string `json:"origin"`
			}
			if err := json.Unmarshal(r.Payload, &identity); err != nil {
				return err
			}
			if identity.Purpose == "workflow_node" || identity.Origin == "workflow_node" {
				return product.NewError(product.CodeIncompatibleVersion, "embedded workflow execution facts are no longer supported")
			}
		}
	}
	return nil
}

func applyCommit(v *View, c store.Commit) error {
	if err := ValidateCodeCommitCompatibility(c); err != nil {
		return err
	}
	if err := validateResourceHoldReleaseCommit(c); err != nil {
		return err
	}
	if err := validateApprovalCommit(v, c); err != nil {
		return err
	}
	if err := validateChildResumeCommit(v, c); err != nil {
		return err
	}
	if err := validateChildCompletionCommit(v, c); err != nil {
		return err
	}
	if err := validateChildAttemptCommit(v, c); err != nil {
		return err
	}
	if err := validateInvocationBudgetCommit(v, c); err != nil {
		return err
	}
	consumesHostCommands := false
	for _, r := range c.ControlRecords {
		if r.Type == "host_command_consumed" {
			consumesHostCommands = true
		}
		if err := applyControl(v, r); err != nil {
			return err
		}
	}
	for _, r := range c.Entries {
		if r.Type != "message" {
			return product.NewError(product.CodeIncompatibleVersion, "unknown required entry")
		}
		var msg agent.AgentMessage
		if err := json.Unmarshal(r.Payload, &msg); err != nil {
			return err
		}
		if err := msg.Validate(); err != nil {
			return err
		}
		if _, exists := v.HostCommands[msg.ID]; exists && (!consumesHostCommands || v.HostCommandConsumptions[msg.ID] != c.CommitSeq) {
			return product.NewError(product.CodeStateConflict, "host command requires an explicit context consumption commit")
		}
		if r.ParentID != v.LeafID {
			return product.NewError(product.CodeIncompatibleVersion, "history parent is not the selected leaf")
		}
		if _, exists := v.Nodes[r.ID]; exists {
			return product.NewError(product.CodeIncompatibleVersion, "duplicate history entry")
		}
		v.Messages = append(v.Messages, msg)
		indexEntry(v, r.ID, r.ParentID, c.CommitSeq)
		v.LeafID = r.ID
	}
	v.LastSeq = c.CommitSeq
	for _, ev := range c.Events {
		if ev.DurableSeq != nil {
			v.Cursor = *ev.DurableSeq
		}
		v.Events = append(v.Events, ev)
	}
	v.ActiveTrace = ""
	for id, tr := range v.Traces {
		if tr.Started && !terminal(tr.State) {
			if v.ActiveTrace != "" && v.ActiveTrace != id {
				return product.NewError(product.CodeIncompatibleVersion, "multiple active traces")
			}
			v.ActiveTrace = id
		}
	}
	return nil
}
func applyControl(v *View, r store.Record) error {
	if r.Version != 1 {
		return product.NewError(product.CodeIncompatibleVersion, "unknown control version")
	}
	switch r.Type {
	case "host_command_consumed":
		return applyHostCommandConsumption(v, r)
	case "host_command_result":
		return applyHostCommandResult(v, r)
	case "execution_policy":
		return applyExecutionPolicy(v, r)
	case "approval_binding", "approval_decision", "approval_claim":
		return applyApprovalRecord(v, r)
	case "direct_resume":
		return applyDirectResume(v, r)
	case "resumed_execution":
		return applyResumedExecution(v, r)
	case "model_attempt", "selection", "frozen_execution", "interaction", "approval", "checkpoint_ref":
		return applyP2Record(v, r)
	case "model_attempt_details":
		return applyAttemptDetails(v, r)
	case "model_attempt_transition":
		return applyAttemptTransition(v, r)
	case "operation":
		return applyOperation(v, r)
	case "todo_update":
		return applyTodoUpdate(v, r)
	case "tool_output_projection":
		return applyToolProjection(v, r)
	case "observation_revision":
		return applyObservation(v, r)
	case "resource_hold_release":
		return applyResourceHoldRelease(v, r)
	case "reconciliation":
		return applyReconciliation(v, r)
	case "invocation":
		return applyInvocation(v, r)
	case "invocation_message":
		return applyInvocationMessage(v, r)
	case "invocation_budget":
		return applyInvocationBudget(v, r)
	case "workflow_node":
		return product.NewError(product.CodeIncompatibleVersion, "embedded workflow records are no longer supported")
	case "input":
		var in InputState
		if err := json.Unmarshal(r.Payload, &in); err != nil {
			return err
		}
		if in.ID != r.ID {
			return fmt.Errorf("input identity mismatch")
		}
		if v.Inputs[in.ID] == nil {
			v.Order = append(v.Order, in.ID)
			switch in.Kind {
			case "steering":
				v.Steering = append(v.Steering, in.ID)
			case "follow_up":
				v.Follow = append(v.Follow, in.ID)
			default:
				v.Independent = append(v.Independent, in.ID)
			}
		}
		v.Inputs[in.ID] = &in
	case "trace", "generation_ref", "queue_hold":
		var tr TraceState
		if err := json.Unmarshal(r.Payload, &tr); err != nil {
			return err
		}
		if !tr.Activity.Known && tr.Started {
			tr.Activity.Unknown = true
		}
		v.Traces[tr.ID] = &tr
		v.Generation = tr.Generation
	case "idempotency":
		var item idemRecord
		if err := json.Unmarshal(r.Payload, &item); err != nil {
			return err
		}
		v.Idem[r.ID] = item
	case "turn":
		var tr agent.TurnRecord
		if err := json.Unmarshal(r.Payload, &tr); err != nil {
			return err
		}
		v.Turns[tr.ID] = tr
	case "tool_call":
		var call agent.ToolRecord
		if err := json.Unmarshal(r.Payload, &call); err != nil {
			return err
		}
		if old, ok := v.Calls[call.Call.CallID]; ok && old.Observation != nil && (call.Observation == nil || *old.Observation != *call.Observation) {
			return product.NewError(product.CodeStateConflict, "original tool observation is immutable")
		}
		if err := applyLegacyObservation(v, call); err != nil {
			return err
		}
		v.Calls[call.Call.CallID] = call
	case "budget":
		return json.Unmarshal(r.Payload, &v.Budget)
	case "active_cursor":
		return applyActiveCursor(v, r, v.LastSeq+1)
	default:
		return product.NewError(product.CodeIncompatibleVersion, "unknown required control record")
	}
	return nil
}
func digestCommand(cmd agent.InputCommand) (string, error) {
	if !json.Valid(cmd.Content) {
		return "", product.NewError(product.CodeInvalidArgument, "invalid input content")
	}
	dec := json.NewDecoder(bytes.NewReader(cmd.Content))
	dec.UseNumber()
	var content any
	if err := dec.Decode(&content); err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Kind, TargetTraceID, TargetAgent string
		Content                          any
	}{cmd.Kind, cmd.TargetTraceID, cmd.TargetAgent, content})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}
