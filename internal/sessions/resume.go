package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	goruntime "runtime"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func sameLogicalScope(a, b agent.ExecutionScope) bool {
	a.ExecutionID = b.ExecutionID
	return a == b
}

// GenerationFingerprint is the host's explicit compatibility declaration for
// the application/SDK build and compiled model, tools, hooks and controlled
// backends. An empty declaration
// permits execution and Pause, but never authorizes checkpoint Resume.
func resumeBuildFingerprint(opts Options) string {
	configured, ok := opts.Model.(interface{ Configuration() llm.ModelConfig })
	if !ok || opts.GenerationFingerprint == "" || configured.Configuration().Version == "" {
		return ""
	}
	type functionShape struct {
		Name                 string
		Run, Output, Resolve bool
		Prepare, Before      int
	}
	functions := make([]functionShape, 0, len(opts.Tools))
	for _, def := range opts.Tools {
		functions = append(functions, functionShape{def.Name, def.Run != nil, def.RunWithOutput != nil, def.ResolveExecution != nil, len(def.PrepareArguments), len(def.BeforeCall)})
	}
	raw, err := json.Marshal(struct {
		Runtime, OS, Arch, Eino, Codec, Implementation, Instruction string
		Model                                                       llm.ModelConfig
		Functions                                                   []functionShape
		Files, Process, Artifacts                                   bool
	}{runtimeFingerprint(), goruntime.GOOS, goruntime.GOARCH, "0.9.21", "1", opts.GenerationFingerprint, opts.Instruction, configured.Configuration(), functions,
		opts.Operations.Files != nil, opts.Operations.Process != nil, opts.Operations.Artifacts != nil})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "resume-v1:" + hex.EncodeToString(sum[:])
}

func executionManifest(opts Options, generation string) (capabilityManifest, error) {
	// Pause must remain available to older direct Start callers. Resume separately
	// checks that the actual ToolInfos agree with these implementation definitions.
	opts.ToolInfos = nil
	decls, err := alignTools(&opts)
	if err != nil {
		return capabilityManifest{}, err
	}
	return buildManifest(generation, decls, opts.GenerationFingerprint)
}

func (rt *runtime) pauseReference(frame *execution, input agent.InputRef, ref agent.CheckpointBlobRef, view state.View) (state.CheckpointRef, error) {
	manifest, err := executionManifest(rt.opts, frame.scope.Generation)
	if err != nil {
		return state.CheckpointRef{}, err
	}
	if rt.opts.StateRoot != "" && rt.opts.StateRoot != "memory" {
		manifest, err = loadManifest(rt.opts.StateRoot, rt.opts.SessionID, frame.scope.Generation)
		if err != nil {
			return state.CheckpointRef{}, err
		}
	}
	tr := view.Traces[frame.scope.TraceID]
	cp := state.CheckpointRef{ID: agent.MustID(), BlobHash: ref.Hash, BlobSize: ref.Size, Scope: frame.scope, Input: input, Target: tr.Target,
		ProjectionRevision: view.LastSeq, HistoryCommit: view.LastSeq, LeafID: view.LeafID, SelectionRevision: frame.turnSelectionRevision,
		EinoVersion: "0.9.21", CodecVersion: "1", BuildCompatibility: resumeBuildFingerprint(rt.opts),
		EnvironmentFingerprint: rt.opts.Workspace + "\n" + rt.opts.ResourceEnvironment, ManifestHash: manifest.Hash}
	cp.Scope.TurnID = frame.turnID
	var latestAttempt uint64
	for _, attempt := range view.ModelAttempts {
		if sameLogicalScope(attempt.Scope, cp.Scope) && attempt.Attempt > latestAttempt {
			latestAttempt, cp.ModelConfigVersion = attempt.Attempt, attempt.ModelConfigVersion
		}
	}
	if turn, ok := view.Turns[frame.turnID]; ok && !turn.Ended {
		cp.UnfinishedTurnIDs = []string{frame.turnID}
		cp.CallIDs = append([]string(nil), turn.CallIDs...)
	}
	return cp, nil
}

// matchesCallScope accepts only the original calls named by this resumed
// checkpoint. It is not used for fact admission: old ExecutionID facts remain
// rejected by matchesExecution before these original identities are examined.
func (rt *runtime) matchesCallScope(scope agent.ExecutionScope, call agent.ToolRecord) bool {
	if rt.active == nil {
		return false
	}
	if call.Scope == scope && rt.matchesExecution(scope) {
		return true
	}
	resume := rt.active.resume
	if resume == nil || !sameLogicalScope(call.Scope, resume.Scope) {
		return false
	}
	allowed := false
	for _, id := range resume.CallIDs {
		if id == call.Call.CallID {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	if scope == call.Scope {
		return true
	} // process-local execution ticket
	original := call.Scope
	original.ExecutionID = rt.active.scope.ExecutionID
	return scope == original && rt.matchesExecution(scope)
}

type ResumeCommand struct {
	TraceID          string `json:"traceId"`
	ExpectedRevision uint64 `json:"expectedRevision"`
	IdempotencyKey   string `json:"idempotencyKey,omitempty"`
}

// Resume returns the durable acceptance receipt. Completion is queried through
// GetOperation; the request context does not own the resumed worker's lifetime.
func (s *AgentSession) Resume(ctx context.Context, cmd ResumeCommand) (state.OperationReceipt, error) {
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		operation := state.OperationCommand{Principal: rt.opts.Principal, Kind: "resume", Target: cmd.TraceID, ExpectedRevision: cmd.ExpectedRevision, IdempotencyKey: cmd.IdempotencyKey}
		if receipt, found, err := rt.manager.FindOperation(operation); found || err != nil {
			return receipt, err
		}
		view := rt.manager.View()
		if cmd.ExpectedRevision != view.LastSeq {
			return nil, product.NewError(product.CodeStateConflict, "session revision changed")
		}
		cp, err := rt.validateResume(ctx, cmd.TraceID, view)
		if err != nil {
			return nil, err
		}
		tr := view.Traces[cmd.TraceID]
		executionID := agent.MustID()
		receipt, err := rt.manager.CommitResume(ctx, operation, cp.ID, executionID)
		if err != nil {
			return nil, err
		}
		workerCtx, cancel := context.WithCancel(context.Background())
		scope := cp.Scope
		scope.ExecutionID = executionID
		frame := &execution{scope: scope, ctx: workerCtx, cancel: cancel, done: make(chan struct{}), budget: agent.NewBudget(tr.Limits),
			turnID: cp.Scope.TurnID, turnSelectionRevision: cp.SelectionRevision, resume: &cp, resumeID: receipt.OperationID}
		frame.budget.Restore(tr.Usage)
		rt.setBudgetPersistence(frame)
		rt.active = frame
		go rt.runSegment(frame, cp.Input.InputID)
		return receipt, nil
	})
	if err != nil {
		return state.OperationReceipt{}, err
	}
	return value.(state.OperationReceipt), nil
}
