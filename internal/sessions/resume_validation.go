package sessions

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

func incompatibleResume(reason string) error {
	return product.NewError(product.CodeIncompatibleResume, reason)
}

// validateResume runs in the mailbox against one committed view. It deliberately
// supports only Graceful Pause of this static main agent; interaction decisions,
// reconciliation merges and extension/workflow state need their later steps.
func (rt *runtime) validateResume(ctx context.Context, traceID string, view state.View) (state.CheckpointRef, error) {
	fail := func(reason string) (state.CheckpointRef, error) {
		return state.CheckpointRef{}, incompatibleResume(reason)
	}
	tr := view.Traces[traceID]
	if tr == nil {
		return state.CheckpointRef{}, product.NewError(product.CodeNotFound, "trace not found")
	}
	if rt.active != nil || tr.State != "paused" || !tr.Started || tr.Settled || !tr.ExecutionStopped || view.ActiveTrace != traceID {
		return fail("trace has no safely paused execution")
	}
	if view.HasUnresolvedEffects() {
		return state.CheckpointRef{}, product.NewError(product.CodeReconciliationRequired, "unknown tool effects block resume")
	}
	cp, ok := view.Checkpoints[tr.CheckpointID]
	if !ok || cp.Scope.SessionID != rt.opts.SessionID || cp.Scope.BranchID != view.BranchID || cp.Scope.TraceID != traceID || cp.Scope.InvocationID != tr.InvocationID || cp.Scope.ExecutionID != tr.ExecutionID || cp.Scope.Generation != tr.Generation || cp.Scope.Generation != rt.generation || cp.Target != tr.Target || cp.Target.Version == "" || cp.Target.Generation != rt.generation {
		return fail("checkpoint target or execution binding differs")
	}
	// Only the exact saved agent definition may resume; workflows use their
	// own node-level recovery and never this runner checkpoint.
	if def, err := rt.definitionFor(cp.Target); err != nil || def.Kind != agent.AgentKindAgent {
		return fail("checkpoint target is not registered in this build")
	}
	build := resumeBuildFingerprint(rt.opts)
	if build == "" || cp.BuildCompatibility != build || cp.EinoVersion != "0.9.21" || cp.CodecVersion != "1" || cp.EnvironmentFingerprint != rt.opts.Workspace+"\n"+rt.opts.ResourceEnvironment {
		return fail("checkpoint implementation or environment is incompatible")
	}
	copyOpts := rt.opts
	decls, err := alignTools(&copyOpts)
	if err != nil {
		return fail("current tool inventory is incompatible")
	}
	manifest, err := buildManifest(rt.generation, decls, rt.opts.GenerationFingerprint)
	if err != nil || manifest.Hash != cp.ManifestHash {
		return fail("checkpoint generation is unavailable or incompatible")
	}
	if rt.opts.StateRoot != "" && rt.opts.StateRoot != "memory" {
		saved, err := loadManifest(rt.opts.StateRoot, rt.opts.SessionID, rt.generation)
		if err != nil || saved.Hash != cp.ManifestHash {
			return fail("saved generation manifest is unavailable or incompatible")
		}
	}
	checkpointModel, err := rt.modelForCheckpoint(cp, view)
	if err != nil {
		return fail("checkpoint model instance is unavailable")
	}
	configured, hasConfiguration := checkpointModel.(interface{ Configuration() llm.ModelConfig })
	if !hasConfiguration || cp.ModelConfigVersion == "" || configured.Configuration().Version != cp.ModelConfigVersion || cp.ThinkingRef != "" || cp.ExtensionStateCommit != 0 {
		return fail("checkpoint model or extension state is unsupported")
	}
	if err := rt.validateApprovalResume(cp, view); err != nil {
		return state.CheckpointRef{}, err
	}
	input := view.Inputs[cp.Input.InputID]
	if input == nil || input.TraceID != traceID || input.State != "consumed" || cp.Input.TraceID != traceID || cp.Input.Kind != "prompt" {
		return fail("checkpoint original input is unavailable")
	}
	turn, ok := view.Turns[cp.Scope.TurnID]
	if !ok || turn.TraceID != traceID || turn.InvocationID != tr.InvocationID || cp.Scope.TurnID != tr.Usage.ModelCallID || turn.TransportRequests != tr.Usage.ModelRequests {
		return fail("checkpoint model call or budget progress differs")
	}
	if turn.Ended {
		if len(cp.UnfinishedTurnIDs) != 0 || len(cp.CallIDs) != 0 {
			return fail("checkpoint unfinished turn differs")
		}
	} else if len(cp.UnfinishedTurnIDs) != 1 || cp.UnfinishedTurnIDs[0] != turn.ID || !reflect.DeepEqual(cp.CallIDs, turn.CallIDs) {
		return fail("checkpoint unfinished calls differ")
	}
	matchedModel := false
	for _, attempt := range view.ModelAttempts {
		if sameLogicalScope(attempt.Scope, cp.Scope) {
			if attempt.ModelConfigVersion != cp.ModelConfigVersion || view.AttemptResults[attempt.ID].State == "" {
				return fail("checkpoint model configuration or attempt is unfinished")
			}
			matchedModel = true
		}
	}
	if !matchedModel {
		return fail("checkpoint original model identity is unavailable")
	}
	policy := view.ExecutionPolicy
	if _, err := state.NormalizeExecutionPolicy(policy); err != nil {
		return state.CheckpointRef{}, err
	}
	for _, id := range cp.CallIDs {
		call, ok := view.Calls[id]
		if !ok || !sameLogicalScope(call.Scope, cp.Scope) || !acceptedAttemptForCall(view, call) {
			return fail("checkpoint call is not the original accepted call")
		}
		if call.Observation != nil {
			continue
		}
		for _, def := range rt.opts.Tools {
			if def.Name != call.Call.Name {
				continue
			}
			description := def.Execution
			if frozen, ok := view.FrozenExecutions["execution:"+id]; ok {
				if frozen.PolicyRef != policy.Ref {
					return state.CheckpointRef{}, product.NewError(product.CodePermissionDenied, "execution policy changed after freezing")
				}
				description.Effect, description.RequestedGrantRef = frozen.Effect, frozen.RequestedGrantRef
			}
			if policy.SandboxMode == "read-only" && description.Effect != "read" && description.Effect != "none" || policy.ApprovalPolicy == "never" && description.RequestedGrantRef != "" {
				return state.CheckpointRef{}, product.NewError(product.CodePermissionDenied, "current execution policy denies the paused call")
			}
		}
	}
	stored, err := rt.opts.Store.Load(ctx, rt.opts.SessionID)
	if err != nil {
		return state.CheckpointRef{}, err
	}
	if err := validateCheckpointProgress(cp, stored, view, rt.opts.Tools); err != nil {
		return state.CheckpointRef{}, err
	}
	blobs, ok := rt.opts.Store.(store.CheckpointBlobs)
	if !ok {
		return fail("checkpoint blob storage is unavailable")
	}
	data, err := blobs.Get(ctx, rt.opts.SessionID, store.BlobRef{Hash: cp.BlobHash, Size: cp.BlobSize})
	if err != nil {
		return fail("checkpoint blob is missing or corrupt")
	}
	if err := einorun.ValidatePausedCheckpoint(data, cp.Input); err != nil {
		return state.CheckpointRef{}, err
	}
	return cp, nil
}

// checkpointIndependentDefault permits only a committed next-trace model
// default. Neither next-turn selections nor activation of any selection can
// change the frozen execution through this exception.
func checkpointIndependentDefault(cp state.CheckpointRef, commit store.Commit, rec store.Record, view state.View) bool {
	var selected state.Selection
	if json.Unmarshal(rec.Payload, &selected) != nil || selected.ID == "" || selected.ID != rec.ID || selected.Kind != "model" || selected.ApplyAt != "next_trace" || selected.Scope != (agent.ExecutionScope{SessionID: cp.Scope.SessionID, BranchID: cp.Scope.BranchID, Generation: cp.Scope.Generation}) {
		return false
	}
	persisted, ok := view.Selections[selected.ID]
	if !ok {
		return false
	}
	persisted.State = selected.State
	if !reflect.DeepEqual(persisted, selected) {
		return false
	}
	op, ok := view.Operations[selected.OperationID]
	if !ok || op.Kind != "select_default_model" || op.SessionID != cp.Scope.SessionID || op.Receipt.Target != "next_trace" || op.Receipt.AcceptedCommit != selected.Revision {
		return false
	}
	switch selected.State {
	case "pending":
		return selected.Revision == commit.CommitSeq
	case "superseded":
		if selected.Revision >= commit.CommitSeq {
			return false
		}
		for _, replacement := range commit.ControlRecords {
			var next state.Selection
			if replacement.Type == "selection" && json.Unmarshal(replacement.Payload, &next) == nil && next.State == "pending" && next.Supersedes == selected.ID && checkpointIndependentDefault(cp, commit, replacement, view) {
				return true
			}
		}
	}
	return false
}

func validateCheckpointProgress(cp state.CheckpointRef, stored store.StoredSession, view state.View, definitions []tools.Definition) error {
	if cp.HistoryCommit == 0 || cp.ProjectionRevision != cp.HistoryCommit || cp.LeafID != view.LeafID || cp.SelectionRevision == 0 {
		return incompatibleResume("checkpoint history projection differs")
	}
	association, selection := false, false
	var last uint64
	for _, commit := range stored.Commits {
		last = commit.CommitSeq
		for _, rec := range commit.ControlRecords {
			if rec.Type == "turn" && rec.ID == cp.Scope.TurnID && !selection {
				selection = commit.ExpectedPreviousSeq == cp.SelectionRevision
				if !selection {
					return incompatibleResume("checkpoint original selection differs")
				}
			}
		}
		if commit.CommitSeq <= cp.HistoryCommit {
			continue
		}
		if len(commit.Entries) != 0 {
			return incompatibleResume("new history exists after checkpoint")
		}
		for _, rec := range commit.ControlRecords {
			if commit.CommitSeq == cp.HistoryCommit+1 && rec.Type == "checkpoint_ref" && rec.ID == cp.ID {
				association = true
				continue
			}
			switch rec.Type {
			case "operation", "trace", "execution_policy", "idempotency", "generation_ref", "queue_hold":
				// These are control-only changes. Current trace, policy, generation,
				// stop proof and checkpoint identity were checked above.
			case "host_command_result":
				if !checkpointHostCommandResult(cp, commit, rec, view) {
					return incompatibleResume("host command result changed checkpoint history or identity")
				}
			case "selection":
				if !checkpointIndependentDefault(cp, commit, rec, view) && !checkpointPendingTurnSelection(cp, commit, rec, view, definitions) {
					return incompatibleResume("selection changed checkpoint execution")
				}
			case "approval_binding":
				var binding state.ApprovalBinding
				if commit.CommitSeq != cp.HistoryCommit+1 || json.Unmarshal(rec.Payload, &binding) != nil || binding.CheckpointID != cp.ID || binding.ID != cp.ID+":"+binding.InteractionID {
					return incompatibleResume("approval target changed after checkpoint")
				}
			case "approval_decision":
				var answer state.ApprovalDecision
				if json.Unmarshal(rec.Payload, &answer) != nil || answer.BindingID != cp.ID+":"+answer.InteractionID {
					return incompatibleResume("approval decision is for another checkpoint")
				}
			case "input":
				var input state.InputState
				if json.Unmarshal(rec.Payload, &input) != nil || input.State != "pending" {
					return incompatibleResume("input was consumed after checkpoint")
				}
			default:
				return incompatibleResume("execution progress changed after checkpoint")
			}
		}
	}
	if last != view.LastSeq || !association || !selection {
		return incompatibleResume("checkpoint association or journal progress is unavailable")
	}
	return nil
}
