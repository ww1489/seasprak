package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

func applyCommit(v *View, c store.Commit) error {
	for _, r := range c.ControlRecords {
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
		if r.ParentID != v.LeafID {
			return product.NewError(product.CodeIncompatibleVersion, "history parent is not the selected leaf")
		}
		v.Messages = append(v.Messages, msg)
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
	case "execution_policy":
		return applyExecutionPolicy(v, r)
	case "resumed_execution":
		return applyResumedExecution(v, r)
	case "model_attempt", "frozen_execution", "interaction", "approval", "checkpoint_ref":
		return applyP2Record(v, r)
	case "model_attempt_details":
		return applyAttemptDetails(v, r)
	case "model_attempt_transition":
		return applyAttemptTransition(v, r)
	case "operation":
		return applyOperation(v, r)
	case "observation_revision":
		return applyObservation(v, r)
	case "reconciliation":
		return applyReconciliation(v, r)
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
		var cursor struct{ BranchID, LeafID string }
		if err := json.Unmarshal(r.Payload, &cursor); err != nil {
			return err
		}
		v.BranchID = cursor.BranchID
		v.LeafID = cursor.LeafID
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
