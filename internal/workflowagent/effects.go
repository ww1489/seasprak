package workflowagent

import (
	"context"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
)

func (w *WorkflowAgent) activeScopeLocked(scope agent.ExecutionScope) error {
	if w.active == nil || !scopeRoot(scope, w.opts.RunID) || scope.ExecutionID != w.active.scope.ExecutionID || scope.Generation != w.binding || !w.state.validNodeScope(scope) {
		return product.NewError(product.CodeStateConflict, "workflow scope is no longer active")
	}
	return nil
}
func (w *WorkflowAgent) CommitFact(ctx context.Context, scope agent.ExecutionScope, fact agent.Fact) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.activeScopeLocked(scope); err != nil {
		return err
	}
	switch fact.Kind {
	case "model_attempt_started":
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.active.ctx.Err(); err != nil {
			return err
		}
		var p agent.ModelAttemptIdentity
		if err := decode(fact.Payload, &p); err != nil {
			return err
		}
		attempt := attemptRecord{Scope: scope, Identity: p, Status: "started"}
		if err := w.commitLocked(ctx, []storage.Record{record("workflow_model_attempt", p.ID, attempt)}, []agent.Event{w.event("model.attempt.started", scope.NodeExecutionID, map[string]any{"attemptId": p.ID, "modelCallId": p.ModelCallID})}); err != nil {
			return err
		}
		seq := uint64(0)
		w.publishLocked(agent.Event{SchemaVersion: 1, Type: "message.started", Scope: w.eventScope(scope.NodeExecutionID), StreamID: p.StreamID, ChunkSeq: &seq, OccurredAt: time.Now().UTC(), Payload: fact.Payload})
		return nil
	case "assistant":
		var result modelResult
		if err := decode(fact.Payload, &result); err != nil {
			return err
		}
		if result.AttemptID == "" && ctx.Err() != nil {
			return nil
		}
		attempt, exists := w.state.Attempts[result.AttemptID]
		if !exists || attempt.Scope != scope {
			return product.NewError(product.CodeStateConflict, "model attempt identity is unavailable")
		}
		attempt.Status = result.Status
		attempt.Result = &result
		if err := w.commitLocked(context.WithoutCancel(ctx), []storage.Record{record("workflow_model_attempt", result.AttemptID, attempt)}, []agent.Event{w.event("model.attempt.finished", scope.NodeExecutionID, map[string]any{"attemptId": result.AttemptID, "status": result.Status, "errorCode": result.Details.FailureCode})}); err != nil {
			return err
		}
		delete(w.transient.Models, attempt.Identity.StreamID)
		delete(w.streamSeq, attempt.Identity.StreamID)
		return nil
	case "model_stream_snapshot":
		return w.modelSnapshotLocked(ctx, scope, fact.Payload)
	case "tool_frozen":
		var f agent.FrozenExecution
		if err := decode(fact.Payload, &f); err != nil {
			return err
		}
		if f.NodeExecutionID != scope.NodeExecutionID {
			return product.NewError(product.CodePermissionDenied, "frozen descriptor has another node")
		}
		registered := false
		for _, def := range w.opts.Tools {
			if def.Name == f.Tool && def.Version == f.ToolVersion && rawHash(def.Schema) == f.SchemaHash {
				registered = true
			}
		}
		if !registered {
			return product.NewError(product.CodePermissionDenied, "frozen tool binding is unavailable")
		}
		if old, ok := w.state.Frozen[f.ID]; ok {
			if old.Hash != f.Hash {
				return product.NewError(product.CodeIncompatibleResume, "frozen execution changed")
			}
			return nil
		}
		return w.commitLocked(context.WithoutCancel(ctx), []storage.Record{record("workflow_frozen", f.ID, f)}, nil)
	case "tool_intent":
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.active.ctx.Err(); err != nil {
			return err
		}
		if fact.Budget == nil {
			return product.NewError(product.CodePermissionDenied, "tool intent requires budget candidate")
		}
		var call agent.FrozenCall
		if err := decode(fact.Payload, &call); err != nil {
			return err
		}
		f, ok := w.state.Frozen["execution:"+call.CallID]
		if !ok || f.NodeExecutionID != scope.NodeExecutionID {
			return product.NewError(product.CodePermissionDenied, "tool claim lacks frozen binding")
		}
		decision, err := w.policyLocked(ctx, f, false)
		if err != nil {
			return err
		}
		if decision != agent.DecisionAllow {
			return product.NewError(product.CodePermissionDenied, "tool claim has no current permission")
		}
		n := w.state.Nodes[scope.NodeExecutionID]
		aggregate := w.state.Usage
		aggregate.ToolExecutions += fact.Budget.ToolExecutions - n.Usage.ToolExecutions
		if aggregate.ToolExecutions > w.state.Initial.Limits.TraceToolCalls {
			return product.NewError(product.CodeBudgetExhausted, "workflow tool budget exhausted")
		}
		p := toolIntent{Scope: f.Scope, Call: call, Budget: budgetRecord{NodeID: n.ID, Local: *fact.Budget, Aggregate: aggregate}}
		if err := w.commitLocked(ctx, []storage.Record{record("workflow_tool_intent", call.CallID, p)}, []agent.Event{w.event("tool.requested", n.ID, map[string]any{"toolCallId": call.CallID, "state": "claimed"})}); err != nil {
			return err
		}
		if f.RequestedGrantRef != "" {
			for _, p := range w.approvals {
				if p.frozen.Hash == f.Hash && p.view.ToolCallID == f.CallID && p.decision == "allowed-once" {
					p.claimedExecution = w.active.scope.ExecutionID
					p.view.State = "claimed"
				}
			}
		}
		return nil
	case "tool_observation":
		var c agent.ToolRecord
		if err := decode(fact.Payload, &c); err != nil {
			return err
		}
		if c.Scope.NodeExecutionID != scope.NodeExecutionID {
			return product.NewError(product.CodePermissionDenied, "observation has another node")
		}
		if err := w.commitLocked(context.WithoutCancel(ctx), []storage.Record{record("workflow_tool_observation", c.Call.CallID, c)}, []agent.Event{w.event("tool.finished", scope.NodeExecutionID, map[string]any{"toolCallId": c.Call.CallID, "status": c.Observation.Status, "sideEffect": c.Observation.SideEffect})}); err != nil {
			return err
		}
		for key, delta := range w.transient.Tools {
			if delta.ToolCallID == c.Call.CallID {
				delete(w.transient.Tools, key)
				delete(w.streamSeq, key)
			}
		}
		return nil
	case "tool_output_projection":
		var p agent.ToolOutputProjection
		if err := decode(fact.Payload, &p); err != nil {
			return err
		}
		call, ok := w.state.Calls[p.CallID]
		if !ok || call.Scope.NodeExecutionID != scope.NodeExecutionID {
			return product.NewError(product.CodePermissionDenied, "tool projection has another node")
		}
		return w.commitLocked(context.WithoutCancel(ctx), []storage.Record{record("workflow_tool_projection", p.CallID, p)}, nil)
	case "tool_output":
		return w.toolOutputLocked(ctx, scope, fact.Payload)
	default:
		return product.NewError(product.CodeUnsupportedCapability, "unsupported workflow execution fact")
	}
}
func (w *WorkflowAgent) LookupWorkflowTool(ctx context.Context, scope agent.ExecutionScope, callID string) (agent.ToolRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.activeScopeLocked(scope); err != nil {
		return agent.ToolRecord{}, err
	}
	c, ok := w.state.Calls[callID]
	n := w.state.Nodes[scope.NodeExecutionID]
	if !ok || callID != n.ToolCallID || c.Scope.NodeExecutionID != n.ID || c.Scope.InvocationID != scope.InvocationID || n.State != "accepted" || c.Call.ProviderCallID != "" {
		return agent.ToolRecord{}, product.NewError(product.CodePermissionDenied, "workflow call was not accepted")
	}
	if c.Claimed && (c.Observation == nil || c.Observation.SideEffect == "unknown" || c.Observation.Status == "outcome_unknown") {
		return agent.ToolRecord{}, product.NewError(product.CodeReconciliationRequired, "workflow tool has unresolved effects")
	}
	if c.Observation != nil {
		obs := *c.Observation
		c.Observation = &obs
	}
	if c.Projection != nil {
		projection := *c.Projection
		c.Projection = &projection
	}
	return c, nil
}
func (w *WorkflowAgent) LookupFrozenExecution(ctx context.Context, scope agent.ExecutionScope, callID string) (agent.FrozenExecution, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.activeScopeLocked(scope); err != nil {
		return agent.FrozenExecution{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return agent.FrozenExecution{}, false, err
	}
	n := w.state.Nodes[scope.NodeExecutionID]
	call, ok := w.state.Calls[callID]
	if !ok || n.State != "accepted" || n.ToolCallID != callID || call.Claimed || call.Observation != nil || call.Scope.NodeExecutionID != scope.NodeExecutionID || call.Scope.InvocationID != scope.InvocationID {
		return agent.FrozenExecution{}, false, product.NewError(product.CodePermissionDenied, "workflow frozen call is not pending")
	}
	f, found := w.state.Frozen["execution:"+callID]
	return f.Clone(), found, nil
}

func (w *WorkflowAgent) ExecutionPolicyRef(ctx context.Context, scope agent.ExecutionScope) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.activeScopeLocked(scope); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return w.state.Initial.Policy.Ref, nil
}
func (w *WorkflowAgent) ExecutionSandboxMode(ctx context.Context, scope agent.ExecutionScope) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.activeScopeLocked(scope); err != nil {
		return "", err
	}
	return w.state.Initial.Policy.SandboxMode, nil
}

type workflowAuthorizer struct {
	owner *WorkflowAgent
	scope agent.ExecutionScope
}

func (a workflowAuthorizer) Authorize(ctx context.Context, call agent.FrozenCall) (agent.Decision, error) {
	w := a.owner
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.activeScopeLocked(a.scope); err != nil {
		return agent.DecisionDeny, err
	}
	f, ok := w.state.Frozen["execution:"+call.CallID]
	if !ok || f.Tool != call.Name || f.Hash != call.Hash || string(f.FinalArguments) != call.Arguments || f.Generation != call.Generation || f.NodeExecutionID != a.scope.NodeExecutionID {
		return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "authorization differs from committed descriptor")
	}
	return w.policyLocked(ctx, f, false)
}
func (w *WorkflowAgent) policyLocked(ctx context.Context, f agent.FrozenExecution, claimed bool) (agent.Decision, error) {
	if err := ctx.Err(); err != nil {
		return agent.DecisionCancel, err
	}
	if w.active == nil || w.active.ctx.Err() != nil || w.state.Run.ExecutionStopped || terminal(w.state.Run.State) {
		return agent.DecisionDeny, product.NewError(product.CodeStateConflict, "workflow segment has stopped")
	}
	c, ok := w.state.Calls[f.CallID]
	saved, exists := w.state.Frozen[f.ID]
	n := w.state.Nodes[f.NodeExecutionID]
	sum, err := f.Digest()
	if !ok || !exists || c.Scope != f.Scope || c.Claimed != claimed || c.Observation != nil || n.State != "accepted" || n.ToolCallID != f.CallID || saved.Hash != f.Hash || err != nil || sum != f.Hash || f.Hash == "" {
		return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "tool is not a pending frozen workflow call")
	}
	p := w.state.Initial.Policy
	if p.Ref != f.PolicyRef {
		return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "execution policy changed")
	}
	if p.SandboxMode == "read-only" && f.Effect != "read" && f.Effect != "none" {
		return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "read-only policy denies side effects")
	}
	if f.RequestedGrantRef != "" {
		if p.ApprovalPolicy == "never" {
			return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "approval is not permitted")
		}
		for _, pending := range w.approvals {
			if pending.frozen.Hash != f.Hash || pending.frozen.Scope != f.Scope {
				continue
			}
			now := time.Now()
			if !now.Before(pending.view.ExpiresAt) || now.Before(pending.view.ExpiresAt.Add(-config.ApprovalValidity)) {
				return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "approval expired")
			}
			switch pending.decision {
			case "allowed-once":
				if claimed && pending.claimedExecution == w.active.scope.ExecutionID || !claimed && pending.claimedExecution == "" && pending.stoppedExecution == w.active.from {
					return agent.DecisionAllow, nil
				}
			case "rejected":
				return agent.DecisionDeny, nil
			case "cancelled":
				return agent.DecisionCancel, nil
			}
		}
		return agent.DecisionAsk, nil
	}
	switch f.BackendID {
	case "trusted-run":
		for _, def := range w.opts.Tools {
			if def.Name == f.Tool && def.Version == f.ToolVersion && rawHash(def.Schema) == f.SchemaHash && (def.Run != nil || def.RunWithOutput != nil) && len(f.Argv) == 0 && f.Cwd == "" && len(f.Mounts) == 0 && f.StdinRef == "" && f.TempRootRef == "" {
				return agent.DecisionAllow, nil
			}
		}
	case "process-operations":
		if w.opts.Operations.Process != nil && len(f.Argv) > 0 {
			return agent.DecisionAllow, nil
		}
	case "file-operations":
		if w.opts.Operations.Files != nil && len(f.Argv) == 0 {
			return agent.DecisionAllow, nil
		}
	case "artifact-store":
		if w.opts.Operations.Artifacts != nil {
			return agent.DecisionAllow, nil
		}
	case "todo-operations":
		if w.opts.Operations.Todos != nil {
			return agent.DecisionAllow, nil
		}
	}
	return agent.DecisionDeny, product.NewError(product.CodeResourceUnavailable, "controlled workflow backend is unavailable")
}
func (w *WorkflowAgent) ValidateExecutionTicket(ctx context.Context, f agent.FrozenExecution) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	decision, err := w.policyLocked(ctx, f, true)
	if err != nil {
		return err
	}
	if decision != agent.DecisionAllow {
		return product.NewError(product.CodePermissionDenied, "workflow ticket no longer has permission")
	}
	return nil
}
func (w *WorkflowAgent) restoreHolds() error {
	var holds []tools.ResourceHold
	w.mu.Lock()
	for id, c := range w.state.Calls {
		if !c.Claimed || c.Observation != nil && c.Observation.SideEffect != "unknown" && c.Observation.Status != "outcome_unknown" && (!c.Observation.Process || c.Observation.Terminated || !c.Observation.Executed && c.Observation.SideEffect == "none") {
			continue
		}
		req := tools.ResourceRequest{Environment: "trusted-injected", Workspace: w.opts.Workspace, Effect: "unknown"}
		if f, ok := w.state.Frozen["execution:"+id]; ok {
			req.Resources = f.Resources
			req.Effect = f.Effect
			req.Concurrency = f.Concurrency
		}
		holds = append(holds, tools.ResourceHold{ID: tools.ResourceHoldID("workflow:"+w.opts.RunID, id), Request: req})
	}
	w.mu.Unlock()
	return w.scheduler.RestoreHolds(holds)
}
