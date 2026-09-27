package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	goruntime "runtime"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func directBuildFingerprint(opts Options) string {
	if opts.GenerationFingerprint == "" {
		return ""
	}
	raw, _ := json.Marshal([]string{"direct-resume-v1", runtimeFingerprint(), goruntime.GOOS, goruntime.GOARCH, opts.GenerationFingerprint})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (rt *runtime) commandWaitBinding(frame *execution, wait *agent.ApprovalWait, v state.View) (state.DirectResumeBinding, error) {
	in, exists := v.Interactions[wait.InteractionID]
	if !exists || in.Scope != frame.scope {
		return state.DirectResumeBinding{}, incompatibleResume("direct approval does not belong to the exited execution")
	}
	manifest, err := executionManifest(rt.opts, frame.scope.Generation)
	if err != nil {
		return state.DirectResumeBinding{}, err
	}
	approval := v.Approvals[in.ApprovalID]
	return state.DirectResumeBinding{ID: agent.MustID(), Scope: frame.scope, InputID: frame.input.InputID, OperationID: in.CallID, CallID: in.CallID, InteractionID: in.ID, ApprovalID: approval.ID, FrozenHash: approval.FrozenHash, BuildCompatibility: directBuildFingerprint(rt.opts), EnvironmentFingerprint: rt.opts.Workspace + "\n" + rt.opts.ResourceEnvironment, ManifestHash: manifest.Hash, HistoryCommit: v.LastSeq, LeafID: v.LeafID}, nil
}

func (rt *runtime) validateCommandResume(ctx context.Context, traceID string, v state.View) (state.DirectResumeBinding, error) {
	fail := func(message string) (state.DirectResumeBinding, error) {
		return state.DirectResumeBinding{}, incompatibleResume(message)
	}
	tr := v.Traces[traceID]
	if tr == nil {
		return state.DirectResumeBinding{}, product.NewError(product.CodeNotFound, "trace not found")
	}
	if rt.active != nil || tr.State != "paused" || !tr.Started || tr.Settled || !tr.ExecutionStopped || v.ActiveTrace != traceID {
		return fail("command has no safely stopped execution")
	}
	if v.HasUnresolvedEffects() {
		return state.DirectResumeBinding{}, product.NewError(product.CodeReconciliationRequired, "unknown effects block command resume")
	}
	b, exists := v.DirectResumes[tr.DirectResumeID]
	if !exists || v.ValidateDirectBinding(b) != nil || b.Scope.SessionID != rt.opts.SessionID || b.Scope.BranchID != v.BranchID || b.Scope.TraceID != traceID || b.Scope.ExecutionID != tr.ExecutionID || b.Scope.Generation != rt.generation || tr.Target.Generation != rt.generation || tr.CheckpointID != "" || b.BuildCompatibility == "" || b.BuildCompatibility != directBuildFingerprint(rt.opts) || b.EnvironmentFingerprint != rt.opts.Workspace+"\n"+rt.opts.ResourceEnvironment || b.LeafID != v.LeafID {
		return fail("direct command implementation or original binding differs")
	}
	call := v.Calls[b.CallID]
	if call.Claimed || call.Observation != nil || v.Inputs[b.InputID].State != "pending" || v.Operations[b.OperationID].State != "running" {
		return fail("direct command is not an unclaimed pending call")
	}
	manifest, err := executionManifest(rt.opts, rt.generation)
	if err != nil || manifest.Hash != b.ManifestHash {
		return fail("direct command generation differs")
	}
	if rt.opts.StateRoot != "" && rt.opts.StateRoot != "memory" {
		saved, err := loadManifest(rt.opts.StateRoot, rt.opts.SessionID, rt.generation)
		if err != nil || saved.Hash != b.ManifestHash {
			return fail("saved direct command generation is unavailable")
		}
	}
	approval := v.Approvals[b.ApprovalID]
	answer, answered := v.ApprovalDecisions[b.ApprovalID]
	binding := v.ApprovalBindings[b.ID+":"+b.InteractionID]
	if !answered {
		return state.DirectResumeBinding{}, product.NewError(product.CodeStateConflict, "direct command approval is unanswered")
	}
	if binding.DirectResumeID != b.ID || binding.CheckpointID != "" || binding.TargetRef != "" || binding.ApprovalID != approval.ID || answer.BindingID != binding.ID || answer.Principal == "" {
		return fail("direct approval decision has no original binding")
	}
	if !rt.approvalNow().Before(approval.ExpiresAt) {
		return state.DirectResumeBinding{}, product.NewError(product.CodePermissionDenied, "direct command approval expired")
	}
	frozen := v.FrozenExecutions[approval.FrozenExecutionID]
	p := v.ExecutionPolicy
	if _, err := state.NormalizeExecutionPolicy(p); err != nil {
		return state.DirectResumeBinding{}, err
	}
	if p.Ref != frozen.PolicyRef || p.ApprovalPolicy == "never" || (p.SandboxMode == "read-only" && frozen.Effect != "read" && frozen.Effect != "none") {
		return state.DirectResumeBinding{}, product.NewError(product.CodePermissionDenied, "current policy denies the original command")
	}
	def, known := findDefinition(rt.opts.Tools, call.Call.Name)
	if !known || def.Version != frozen.ToolVersion || toolArgumentHash(def.Schema) != frozen.SchemaHash {
		return fail("original direct implementation is unavailable")
	}
	if err := directBackendAvailable(rt.opts.Operations, def); err != nil {
		return state.DirectResumeBinding{}, err
	}
	stored, err := rt.opts.Store.Load(ctx, rt.opts.SessionID)
	if err != nil {
		return state.DirectResumeBinding{}, err
	}
	associated := false
	for _, commit := range stored.Commits {
		if commit.CommitSeq <= b.HistoryCommit {
			continue
		}
		if len(commit.Entries) != 0 {
			return fail("command history changed after waiting")
		}
		for _, rec := range commit.ControlRecords {
			switch rec.Type {
			case "direct_resume":
				if commit.CommitSeq != b.HistoryCommit+1 || rec.ID != b.ID {
					return fail("command recovery binding changed")
				}
				associated = true
			case "approval_binding":
				if commit.CommitSeq != b.HistoryCommit+1 || rec.ID != binding.ID {
					return fail("command approval binding changed")
				}
			case "approval_decision":
				var saved state.ApprovalDecision
				if json.Unmarshal(rec.Payload, &saved) != nil || saved.BindingID != binding.ID {
					return fail("command decision binding changed")
				}
			case "operation", "trace", "execution_policy", "queue_hold", "generation_ref", "idempotency":
			case "input":
				var input state.InputState
				if json.Unmarshal(rec.Payload, &input) != nil || input.State != "pending" {
					return fail("input advanced after command wait")
				}
			default:
				return fail("execution progress changed after command wait")
			}
		}
	}
	if !associated || len(stored.Commits) == 0 || stored.Commits[len(stored.Commits)-1].CommitSeq != v.LastSeq {
		return fail("direct wait journal association is unavailable")
	}
	return b, nil
}

func (rt *runtime) resumeCommand(ctx context.Context, cmd ResumeCommand, op state.OperationCommand, v state.View) (state.OperationReceipt, error) {
	b, err := rt.validateCommandResume(ctx, cmd.TraceID, v)
	if err != nil {
		return state.OperationReceipt{}, err
	}
	executionID := agent.MustID()
	receipt, err := rt.manager.CommitCommandResume(ctx, op, b.ID, executionID)
	if err != nil {
		return state.OperationReceipt{}, err
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	scope := b.Scope
	scope.ExecutionID = executionID
	tr := v.Traces[cmd.TraceID]
	frame := &execution{scope: scope, ctx: workerCtx, cancel: cancel, done: make(chan struct{}), budget: agent.NewBudget(tr.Limits), directResume: &b, resumeID: receipt.OperationID}
	frame.budget.Restore(tr.Usage)
	rt.setBudgetPersistence(frame)
	rt.active = frame
	go rt.runSegment(frame, b.InputID)
	return receipt, nil
}
