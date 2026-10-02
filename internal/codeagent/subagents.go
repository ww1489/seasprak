package codeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

// Product delegation tool. It is added to the generation only when a
// delegable target is registered, so older manifests keep their hash.
const (
	delegateToolName    = "delegate_task"
	delegateToolVersion = "delegate-task-v1"
)

// delegateDefinition builds the builtin task tool for the delegable targets.
// Run is a placeholder; the session binds the trusted implementation.
func delegateDefinition(agents []agent.AgentDefinition) (tools.Definition, bool) {
	var names []string
	for _, def := range agents {
		if def.Delegable && def.Name != agent.MainAgentName {
			names = append(names, def.Name)
		}
	}
	if len(names) == 0 {
		return tools.Definition{}, false
	}
	sort.Strings(names)
	schemaText, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent": map[string]any{"type": "string", "enum": names},
			"task":  map[string]any{"type": "string", "minLength": 1},
		},
		"required":             []string{"agent", "task"},
		"additionalProperties": false,
	})
	return tools.Definition{Name: delegateToolName, Version: delegateToolVersion,
		Description: "Delegate an explicit, self-contained task to a registered agent. The agent sees only the task text and returns its final answer.",
		Schema:      schemaText, Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "none", Concurrency: "shared"},
		Run: func(context.Context, json.RawMessage) (string, error) {
			return "", errors.New("session delegation is unavailable")
		}}, true
}

func isDelegateBuiltin(def tools.Definition) bool {
	return def.Name == delegateToolName && def.Version == delegateToolVersion && def.Execution.BackendID == "trusted-run"
}

// withDelegateTool appends the builtin exactly once. A different application
// tool with the reserved name conflicts rather than being shadowed. It then
// checks that every agent's declared child tools exist in this generation.
func withDelegateTool(opts *Options) error {
	if err := appendDelegateTool(opts); err != nil {
		return err
	}
	return validateAgentTools(opts)
}

// validateAgentTools rejects child tool names absent from the generation.
// search_tools needs a parent model Turn, so a child cannot declare it.
func validateAgentTools(opts *Options) error {
	registered := map[string]bool{}
	for _, def := range opts.Tools {
		registered[def.Name] = true
	}
	for _, target := range opts.Agents {
		for _, name := range target.Tools {
			if !registered[name] || name == "search_tools" {
				return product.Errorf(product.CodeInvalidArgument, "agent %s declares an unavailable tool", target.Name)
			}
		}
	}
	return nil
}

func appendDelegateTool(opts *Options) error {
	def, ok := delegateDefinition(opts.Agents)
	if !ok {
		return nil
	}
	for _, existing := range opts.Tools {
		if existing.Name == delegateToolName {
			if isDelegateBuiltin(existing) {
				return nil
			}
			return product.NewError(product.CodeInvalidArgument, "tool name delegate_task is reserved for agent delegation")
		}
	}
	opts.Tools = append(opts.Tools, def)
	if len(opts.ToolInfos) != 0 {
		info, err := toolInfoFromDefinition(def)
		if err != nil {
			return err
		}
		opts.ToolInfos = append(opts.ToolInfos, info)
	}
	return nil
}

// canDelegate reports whether the registered caller has any delegation target.
func (rt *runtime) canDelegate(caller string) bool {
	return len(rt.registry.Delegates(caller)) != 0
}

// delegationSlots is mailbox-owned process-local admission per trace.
type delegationSlots map[string]int

func (rt *runtime) admitDelegation(traceID string, depth int) error {
	if depth >= config.SubagentDepth {
		return product.NewError(product.CodeBudgetExhausted, "delegation nesting limit reached")
	}
	if rt.delegations[traceID] >= config.SubagentConcurrency {
		return product.NewError(product.CodeBudgetExhausted, "delegation concurrency limit reached")
	}
	if rt.delegations == nil {
		rt.delegations = delegationSlots{}
	}
	rt.delegations[traceID]++
	return nil
}

func (rt *runtime) releaseDelegation(traceID string) {
	if rt.delegations[traceID] <= 1 {
		delete(rt.delegations, traceID)
		return
	}
	rt.delegations[traceID]--
}

// childSink keeps child model attempts and assistant candidates private, without
// borrowing parent Turns, selection slots or branch history. Accepted child
// tools use the same controlled executor under the child's invocation scope.
// Only committed candidates and observations enter its local compactor.
type childSink struct {
	rt      *runtime
	allowed map[string]bool
	// compactor collects this child's own conversation and owns its local
	// compaction. It is process-local and writes no durable history.
	compactor *childCompactor
}

func (s *childSink) CommitFact(ctx context.Context, scope agent.ExecutionScope, fact agent.Fact) error {
	switch {
	case fact.Kind == "model_attempt_started":
		return s.startModelAttempt(ctx, scope, fact.Payload)
	case fact.Kind == "assistant":
		return s.saveModelAttempt(ctx, scope, fact.Payload)
	case toolFact(fact.Kind):
		if err := s.rt.CommitFact(ctx, scope, fact); err != nil {
			return err
		}
		if fact.Kind == "tool_observation" {
			s.recordChildToolResult(scope, fact.Payload)
		}
		return nil
	}
	return nil
}

// recordChildToolResult keeps the committed observation the child will actually
// see as its tool result, so its summary material is its real conversation.
func (s *childSink) recordChildToolResult(scope agent.ExecutionScope, payload json.RawMessage) {
	if s.compactor == nil || scope.InvocationID != s.compactor.scope.InvocationID {
		return
	}
	var record agent.ToolRecord
	if json.Unmarshal(payload, &record) != nil || record.Observation == nil || record.Call.ProviderCallID == "" {
		return
	}
	s.compactor.record(childToolResultMessage(s.compactor.scope, record.Call.ProviderCallID, record.Call.Name, record.Observation.ModelContent()))
}

func (s *childSink) LookupTool(ctx context.Context, scope agent.ExecutionScope, providerID string) (agent.ToolRecord, error) {
	return s.rt.lookupChildTool(ctx, scope, providerID)
}

func (s *childSink) LookupToolProjection(ctx context.Context, scope agent.ExecutionScope, callID string) (*agent.ToolOutputProjection, error) {
	return s.rt.LookupToolProjection(ctx, scope, callID)
}

func (s *childSink) ToolSelected(_ context.Context, _ agent.ExecutionScope, name string) (bool, error) {
	return s.allowed[name], nil
}

func (s *childSink) ExecutionPolicyRef(ctx context.Context, scope agent.ExecutionScope) (string, error) {
	return s.rt.ExecutionPolicyRef(ctx, scope)
}

func (s *childSink) ExecutionSandboxMode(ctx context.Context, scope agent.ExecutionScope) (string, error) {
	return s.rt.ExecutionSandboxMode(ctx, scope)
}

func (s *childSink) ValidateExecutionTicket(ctx context.Context, frozen agent.FrozenExecution) error {
	return s.rt.ValidateExecutionTicket(ctx, frozen)
}

// matchesDelegatedChild accepts a running delegated invocation of the active
// trace whose parent chain reaches the active root invocation through running
// invocations. It covers ordinary agent children at any admitted depth
// and is used only by the controlled tool pipeline ports.
func (rt *runtime) matchesDelegatedChild(scope agent.ExecutionScope) bool {
	if rt.active == nil || scope.ExecutionID == "" || scope.InvocationID == "" || scope.InvocationID == rt.active.scope.InvocationID {
		return false
	}
	inv, ok := rt.manager.Invocation(scope.InvocationID)
	if !ok || inv.State != "running" || inv.TraceID != rt.active.scope.TraceID || scope.ParentInvocationID != inv.ParentInvocationID {
		return false
	}
	for hops := 0; inv.ParentInvocationID != rt.active.scope.InvocationID; hops++ {
		parent, ok := rt.manager.Invocation(inv.ParentInvocationID)
		if hops > config.SubagentDepth || !ok || parent.State != "running" || parent.TraceID != inv.TraceID {
			return false
		}
		inv = parent
	}
	current := rt.active.scope
	current.InvocationID, current.ParentInvocationID = scope.InvocationID, scope.ParentInvocationID
	current.TurnID, scope.TurnID = "", ""
	current.SelectionRevision, scope.SelectionRevision = 0, 0
	return current == scope
}

// childCallAccepted is the acceptance rule for a call made by a delegated
// agent child: it must be one of the calls registered from that child's own
// model responses, with no Turn, selection or direct operation identity.
func childCallAccepted(v state.View, call agent.ToolRecord) bool {
	inv, ok := v.InvocationForCall(call.Call.CallID)
	return ok && inv.ID == call.Scope.InvocationID && call.Scope.TurnID == "" && call.Scope.SelectionRevision == 0 && call.Call.ProviderCallID != "" && call.Call.OperationID == "" && call.Call.SelectionRevision == 0
}

// lookupChildTool resolves the latest call with providerID accepted by the
// child invocation. Provider IDs may repeat across responses; the executor
// always runs the calls of the response it has just accepted.
func (rt *runtime) lookupChildTool(ctx context.Context, scope agent.ExecutionScope, providerID string) (agent.ToolRecord, error) {
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		if !rt.matchesDelegatedChild(scope) {
			return nil, product.NewError(product.CodeStateConflict, "child tool execution is not active")
		}
		v := rt.manager.View()
		inv := v.Invocations[scope.InvocationID]
		for i := len(inv.CallIDs) - 1; i >= 0; i-- {
			call := v.Calls[inv.CallIDs[i]]
			if call.Call.ProviderCallID != providerID || call.Scope != scope {
				continue
			}
			if v.ReconciliationUnresolved(call.Call.CallID) {
				return nil, product.NewError(product.CodeReconciliationRequired, "tool result has conflicting evidence")
			}
			if effective, ok := v.EffectiveObservation(call.Call.CallID); ok {
				observation := effective.Observation
				call.Observation = &observation
			}
			return call, nil
		}
		return nil, product.NewError(product.CodeStateConflict, "tool call was not accepted")
	})
	if err != nil {
		return agent.ToolRecord{}, err
	}
	return value.(agent.ToolRecord), nil
}

// childRecoveryFingerprint requires the same explicit compatibility declaration
// as checkpoint Resume and also binds the child's full model configuration.
func (rt *runtime) childRecoveryFingerprint(def agent.AgentDefinition) string {
	build := resumeBuildFingerprint(rt.opts)
	configured, ok := def.Model.(interface{ Configuration() llm.ModelConfig })
	if build == "" || !ok || configured.Configuration().Version == "" {
		return ""
	}
	manifest, err := executionManifest(rt.opts, rt.generation)
	if err != nil {
		return ""
	}
	raw, err := json.Marshal([]any{build, configured.Configuration(), manifest.Hash, rt.opts.Workspace, rt.opts.ResourceEnvironment})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// validateChildResume runs in the mailbox against durable invocation facts.
// This restart is intentionally limited to one root child with no claimed
// child tools; an already claimed nested delegation cannot be replayed.
func (rt *runtime) validateChildResume(v state.View, traceID string) (state.Invocation, error) {
	tr := v.Traces[traceID]
	if rt.active != nil || tr == nil || tr.State != "paused" || !tr.Started || tr.Settled || !tr.ExecutionStopped || v.ActiveTrace != traceID || tr.Generation != rt.generation {
		return state.Invocation{}, incompatibleResume("trace has no safely stopped parent execution")
	}
	var candidate state.Invocation
	for _, inv := range v.Invocations {
		if inv.TraceID != traceID || inv.State != "interrupted" || inv.ParentInvocationID != tr.InvocationID {
			continue
		}
		parent, ok := v.Calls[inv.ParentCallID]
		if !ok {
			return state.Invocation{}, incompatibleResume("interrupted child parent call is unavailable")
		}
		if _, safe := resumableInterruptedParent(v, parent); !safe {
			return state.Invocation{}, product.NewError(product.CodeReconciliationRequired, "claimed child tool effects block child restart")
		}
		if candidate.ID != "" {
			return state.Invocation{}, incompatibleResume("multiple interrupted children require independent recovery")
		}
		candidate = inv
	}
	if candidate.ID == "" {
		return state.Invocation{}, incompatibleResume("interrupted child recovery point is unavailable")
	}
	def, err := rt.definitionFor(candidate.Target)
	if err != nil || def.Kind != agent.AgentKindAgent {
		return state.Invocation{}, incompatibleResume("interrupted child target is unavailable")
	}
	if def.Model == nil {
		def.Model = rt.opts.Model
	}
	fingerprint := rt.childRecoveryFingerprint(def)
	if fingerprint == "" || candidate.RecoveryFingerprint != fingerprint || candidate.RecoveryLeafID != v.LeafID {
		return state.Invocation{}, incompatibleResume("child recovery build, model, environment or history differs")
	}
	parent := v.Calls[candidate.ParentCallID]
	turn := v.Turns[parent.Scope.TurnID]
	input := v.Inputs[candidate.RecoveryInputID]
	if parent.Scope.SessionID != rt.opts.SessionID || parent.Scope.BranchID != v.BranchID || parent.Scope.Generation != tr.Generation || turn.ID == "" || turn.Ended || turn.TraceID != tr.ID || turn.InvocationID != tr.InvocationID || !acceptedAttemptForCall(v, parent) || input == nil || input.TraceID != tr.ID || input.State != "consumed" || input.Kind != "prompt" {
		return state.Invocation{}, incompatibleResume("child recovery original input or accepted parent turn differs")
	}
	if tr.Usage.ModelCallID != turn.ID || turn.TransportRequests != tr.Usage.ModelRequests {
		return state.Invocation{}, incompatibleResume("child recovery parent budget progress differs")
	}
	for id, call := range v.Calls {
		if id != parent.Call.CallID && childCallUnknown(v, call) {
			return state.Invocation{}, product.NewError(product.CodeReconciliationRequired, "other unknown tool effects block child restart")
		}
	}
	frozen, ok := v.FrozenExecutions["execution:"+parent.Call.CallID]
	if !ok || frozen.PolicyRef != v.ExecutionPolicy.Ref {
		return state.Invocation{}, product.NewError(product.CodePermissionDenied, "execution policy changed after delegation was frozen")
	}
	model, err := rt.modelForCheckpoint(state.CheckpointRef{Scope: parent.Scope, SelectionRevision: turn.SelectionRevision, ModelConfigVersion: turn.ModelConfigVersion}, v)
	configured, ok := model.(interface{ Configuration() llm.ModelConfig })
	if err != nil || !ok || turn.ModelConfigVersion == "" || configured.Configuration().Version != turn.ModelConfigVersion {
		return state.Invocation{}, incompatibleResume("parent model configuration is unavailable")
	}
	return candidate, nil
}

func interruptedRootChild(v state.View, traceID string) bool {
	tr := v.Traces[traceID]
	if tr == nil {
		return false
	}
	for _, inv := range v.Invocations {
		if inv.TraceID == traceID && inv.ParentInvocationID == tr.InvocationID && inv.State == "interrupted" {
			return true
		}
	}
	return false
}

func (rt *runtime) resumeChild(ctx context.Context, operation state.OperationCommand, view state.View, inv state.Invocation) (state.OperationReceipt, error) {
	parent := view.Calls[inv.ParentCallID]
	var args struct {
		Agent string `json:"agent"`
		Task  string `json:"task"`
	}
	frozen := view.FrozenExecutions["execution:"+parent.Call.CallID]
	if json.Unmarshal(frozen.FinalArguments, &args) != nil || args.Agent != inv.Target.Name || strings.TrimSpace(args.Task) == "" {
		return state.OperationReceipt{}, incompatibleResume("interrupted child arguments are unavailable")
	}
	def, err := rt.definitionFor(inv.Target)
	if err != nil || def.Kind != agent.AgentKindAgent {
		return state.OperationReceipt{}, incompatibleResume("interrupted child target is unavailable")
	}
	if def.Model == nil {
		def.Model = rt.opts.Model
	}
	turn := view.Turns[parent.Scope.TurnID]
	parentModel, err := rt.modelForCheckpoint(state.CheckpointRef{Scope: parent.Scope, SelectionRevision: turn.SelectionRevision, ModelConfigVersion: turn.ModelConfigVersion}, view)
	if err != nil {
		return state.OperationReceipt{}, err
	}
	inputID := inv.RecoveryInputID
	if inputID == "" {
		return state.OperationReceipt{}, incompatibleResume("interrupted child original input is unavailable")
	}
	if err := rt.admitDelegation(inv.TraceID, 0); err != nil {
		return state.OperationReceipt{}, err
	}
	executionID := agent.MustID()
	receipt, err := rt.manager.CommitChildResume(ctx, operation, inv.ID, executionID)
	if err != nil {
		rt.releaseDelegation(inv.TraceID)
		return state.OperationReceipt{}, err
	}
	tr := rt.manager.View().Traces[inv.TraceID]
	workerCtx, cancel := context.WithCancel(context.Background())
	scope := agent.ExecutionScope{SessionID: rt.opts.SessionID, BranchID: view.BranchID, TraceID: tr.ID, InvocationID: tr.InvocationID, ExecutionID: executionID, Generation: tr.Generation}
	frame := &execution{scope: scope, ctx: workerCtx, cancel: cancel, done: make(chan struct{}), budget: agent.NewBudget(tr.Limits), resumeID: receipt.OperationID, currentModel: parentModel,
		turnID: parent.Scope.TurnID, childResume: &childResume{inv: inv, def: def, task: args.Task}}
	frame.budget.Restore(tr.Usage)
	rt.setBudgetPersistence(frame)
	rt.active = frame
	go rt.runSegment(frame, inputID)
	return receipt, nil
}

func (rt *runtime) executeChildResume(frame *execution) error {
	r := frame.childResume
	start := delegateStart{inv: r.inv, def: r.def, scope: agent.ExecutionScope{SessionID: frame.scope.SessionID, BranchID: frame.scope.BranchID, TraceID: frame.scope.TraceID, InvocationID: r.inv.ID, ParentInvocationID: r.inv.ParentInvocationID, ExecutionID: frame.scope.ExecutionID, Generation: frame.scope.Generation}, task: r.task, parent: frame.budget}
	if activity := frame.activity; activity != nil {
		start.remaining = activity.remaining
	}
	text, calls, runErr := rt.runChild(frame.ctx, start)
	return rt.do(context.Background(), func(rt *runtime) error {
		rt.releaseDelegation(r.inv.TraceID)
		if rt.active != frame {
			return product.NewError(product.CodeStateConflict, "child recovery execution changed")
		}
		v := rt.manager.View()
		if frame.ctx.Err() != nil {
			inv := v.Invocations[r.inv.ID]
			inv.State, inv.ModelCalls = "interrupted", inv.ModelCalls+calls
			return errors.Join(runErr, frame.ctx.Err(), rt.manager.SaveInvocation(context.Background(), inv))
		}
		code := ""
		if invocationTreeUnknown(v, r.inv.ID) {
			code = product.CodeReconciliationRequired
			runErr = product.NewError(code, "delegated tool effect is unknown")
		} else if runErr != nil {
			code = product.CodeInternal
			if pe, ok := product.AsError(runErr); ok {
				code = pe.Code
			}
		}
		text, truncated := boundText(text, config.DelegateResultBytes)
		commitErr := rt.manager.CompleteChildResume(context.Background(), r.inv.ID, frame.scope.ExecutionID, text, calls, code, truncated)
		if code == product.CodeReconciliationRequired {
			return errors.Join(runErr, commitErr)
		}
		return commitErr
	})
}

// childCallUnknown reports whether a child call may have produced an effect
// that no durable fact resolves.
func childCallUnknown(v state.View, call agent.ToolRecord) bool {
	id := call.Call.CallID
	if v.ReconciliationUnresolved(id) {
		return true
	}
	if effective, ok := v.EffectiveObservation(id); ok {
		return effective.Observation.SideEffect == "unknown" || effective.Observation.Status == "outcome_unknown"
	}
	if call.Observation != nil {
		return call.Observation.SideEffect == "unknown" || call.Observation.Status == "outcome_unknown"
	}
	return call.Claimed
}

func childCallConfirmed(v state.View, call agent.ToolRecord) bool {
	if effective, ok := v.EffectiveObservation(call.Call.CallID); ok {
		return effective.Observation.SideEffect == "confirmed"
	}
	return call.Observation != nil && call.Observation.SideEffect == "confirmed"
}

type delegateStart struct {
	inv       state.Invocation
	def       agent.AgentDefinition
	scope     agent.ExecutionScope
	task      string
	parent    *agent.BudgetLedger
	remaining func() time.Duration
}

type delegateResult struct {
	Status       string `json:"status"`
	Code         string `json:"code,omitempty"`
	Agent        string `json:"agent,omitempty"`
	InvocationID string `json:"invocationId,omitempty"`
	Result       string `json:"result,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
}

type childResume struct {
	inv  state.Invocation
	def  agent.AgentDefinition
	task string
}

func delegateJSON(r delegateResult) string {
	raw, _ := json.Marshal(r)
	return string(raw)
}

// CanRetainInterruptedTool is the controlled executor's owner check. Only a
// closing parent with a durably safe interrupted child retains its original
// delegate call. Ordinary trusted callbacks and cancelled traces cannot.
func (rt *runtime) CanRetainInterruptedTool(ctx context.Context, scope agent.ExecutionScope, callID string) (bool, error) {
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		v := rt.manager.View()
		call, ok := v.Calls[callID]
		tr := v.Traces[scope.TraceID]
		frozen, bound := v.FrozenExecutions["execution:"+callID]
		if !ok || !bound || !rt.closing || !rt.matchesExecution(scope) || call.Scope != scope || tr == nil || tr.State == "cancelling" || frozen.BackendID != "trusted-run" || frozen.Effect != "none" {
			return false, nil
		}
		_, safe := resumableInterruptedParent(v, call)
		return safe, nil
	})
	if err != nil {
		return false, err
	}
	return value.(bool), nil
}

// runDelegateTask is the trusted Run of the claimed delegate_task call. It
// runs the child synchronously under the tool context, so parent cancellation
// reaches the child. Every known outcome is returned as a structured result;
// only an unexpected internal failure is reported as an execution error.
func (rt *runtime) runDelegateTask(ctx context.Context, raw json.RawMessage) (string, error) {
	scope := einorun.ScopeFromContext(ctx, agent.ExecutionScope{})
	providerID := compose.GetToolCallID(ctx)
	value, err := rt.call(context.WithoutCancel(ctx), func(rt *runtime) (any, error) {
		return rt.startDelegation(ctx, scope, providerID, raw)
	})
	if err != nil {
		if pe, ok := product.AsError(err); ok && pe.Code != product.CodeInternal && pe.Code != product.CodeStorageUnavailable {
			return delegateJSON(delegateResult{Status: "denied", Code: pe.Code}), nil
		}
		return "", err
	}
	start := value.(delegateStart)
	text, calls, runErr := rt.runChild(ctx, start)
	out := delegateResult{Status: "completed", Agent: start.def.Name, InvocationID: start.inv.ID}
	interrupted := false
	switch {
	case runErr != nil && ctx.Err() != nil:
		out.Status, out.Code = "cancelled", "cancelled"
	case runErr != nil:
		out.Status, out.Code = "failed", product.CodeInternal
		var pe *product.Error
		if errors.As(runErr, &pe) {
			out.Code = pe.Code
		}
	default:
		out.Result, out.Truncated = boundText(text, config.DelegateResultBytes)
	}
	inv := start.inv
	inv.State, inv.ModelCalls, inv.Result = out.Status, calls, out.Result
	var unknown bool
	commitErr := rt.do(context.Background(), func(rt *runtime) error {
		rt.releaseDelegation(inv.TraceID)
		v := rt.manager.View()
		unknown = invocationTreeUnknown(v, inv.ID)
		// A parent Close stops the child without a terminal outcome: the
		// invocation stays interrupted (non-terminal) for reopen and evidence.
		if out.Status == "cancelled" && rt.closing {
			if tr := v.Traces[inv.TraceID]; tr != nil && tr.State != "cancelling" {
				inv.State, interrupted = "interrupted", true
			}
		}
		return rt.manager.SaveInvocation(context.Background(), inv)
	})
	if commitErr != nil {
		return "", commitErr
	}
	if interrupted {
		// The executor independently asks the durable owner before retaining
		// this call; unknown child effects always use normal finalization.
		if unknown {
			return "", product.NewError(product.CodeReconciliationRequired, "delegated tool effect is unknown")
		}
		return "", errors.Join(tools.ErrResumableInterruption, ctx.Err())
	}
	// An unresolved child effect makes the parent outcome unknown, so the
	// trusted-run error path records SideEffect unknown and the parent trace
	// requires reconciliation before it can continue.
	if unknown {
		return "", product.NewError(product.CodeReconciliationRequired, "delegated tool effect is unknown")
	}
	return delegateJSON(out), nil
}

// invocationTree lists the invocation and all its descendant invocations.
func invocationTree(v state.View, rootID string) []state.Invocation {
	var out []state.Invocation
	pending := []string{rootID}
	for len(pending) > 0 {
		id := pending[0]
		pending = pending[1:]
		inv, ok := v.Invocations[id]
		if !ok {
			continue
		}
		out = append(out, inv)
		for _, child := range v.Invocations {
			if child.ParentInvocationID == id && child.TraceID == inv.TraceID {
				pending = append(pending, child.ID)
			}
		}
	}
	return out
}

// invocationTreeUnknown reports an unresolved tool effect anywhere below the
// invocation, including nested delegations.
func invocationTreeUnknown(v state.View, rootID string) bool {
	tree := invocationTree(v, rootID)
	for _, call := range v.Calls {
		for _, inv := range tree {
			if call.Scope.InvocationID == inv.ID && call.Scope.TraceID == inv.TraceID && childCallUnknown(v, call) {
				return true
			}
		}
	}
	return false
}

// resumableInterruptedParent identifies the one narrow exception to ordinary
// interrupted-tool finalization. The parent delegate call may stay pending only
// when its original child is interrupted and that child has no descendant
// invocation and no claimed or unknown child tool effect.
func resumableInterruptedParent(v state.View, call agent.ToolRecord) (state.Invocation, bool) {
	return v.InterruptedChildForCall(call.Call.CallID)
}

// startDelegation runs in the mailbox: it validates the claimed call and the
// caller's targets, admits the child and commits the running invocation.
func (rt *runtime) startDelegation(ctx context.Context, scope agent.ExecutionScope, providerID string, raw json.RawMessage) (delegateStart, error) {
	if err := rt.writable(); err != nil {
		return delegateStart{}, err
	}
	// A delegated agent child that declares delegate_task delegates further
	// from its own scope; its claimed call is one of its registered calls.
	nested := !rt.matchesExecution(scope) && rt.matchesDelegatedChild(scope)
	if (!rt.matchesExecution(scope) && !nested) || scope.Generation != rt.generation {
		return delegateStart{}, product.NewError(product.CodeStateConflict, "delegation execution is no longer active")
	}
	if err := ctx.Err(); err != nil {
		return delegateStart{}, product.NewError(product.CodeStateConflict, "delegation was cancelled")
	}
	view := rt.manager.View()
	var callID string
	for _, call := range view.Calls {
		if call.Scope == scope && call.Call.ProviderCallID == providerID && call.Call.Name == delegateToolName && call.Claimed && call.Observation == nil {
			callID = call.Call.CallID
			break
		}
	}
	if callID == "" {
		return delegateStart{}, product.NewError(product.CodePermissionDenied, "delegation requires its claimed model call")
	}
	var args struct {
		Agent string `json:"agent"`
		Task  string `json:"task"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || strings.TrimSpace(args.Task) == "" {
		return delegateStart{}, invalidSelection("invalid delegation arguments")
	}
	tr := view.Traces[scope.TraceID]
	if tr == nil {
		return delegateStart{}, product.NewError(product.CodeNotFound, "trace not found")
	}
	callerTarget := tr.Target
	if nested {
		callerTarget = view.Invocations[scope.InvocationID].Target
	}
	caller, err := rt.definitionFor(callerTarget)
	if err != nil {
		return delegateStart{}, err
	}
	// Depth is the number of delegated invocations on the caller's chain:
	// 0 for the root caller, 1 for its child, and so on.
	depth := 0
	for id := scope.InvocationID; depth <= config.SubagentDepth; depth++ {
		parent, isChild := view.Invocations[id]
		if !isChild {
			break
		}
		id = parent.ParentInvocationID
	}
	var target *agent.AgentDefinition
	for _, def := range rt.registry.Delegates(caller.Name) {
		if def.Name == args.Agent {
			copy := def
			target = &copy
			break
		}
	}
	if target == nil {
		return delegateStart{}, product.NewError(product.CodePermissionDenied, "agent is not a delegation target of the caller")
	}
	if target.Kind != agent.AgentKindAgent {
		return delegateStart{}, product.NewError(product.CodeUnsupportedCapability, "delegation target kind is unsupported")
	}
	identity, err := rt.registry.Target(target.Name, rt.generation)
	if err != nil {
		return delegateStart{}, err
	}
	if target.Model == nil {
		target.Model = rt.opts.Model
	}
	if target.Model == nil {
		return delegateStart{}, product.NewError(product.CodeResourceUnavailable, "model instance is unavailable")
	}
	if err := rt.admitDelegation(scope.TraceID, depth); err != nil {
		return delegateStart{}, err
	}
	inv := state.Invocation{ID: agent.MustID(), ParentInvocationID: scope.InvocationID, ParentCallID: callID, TraceID: scope.TraceID, Target: identity, State: "running",
		RecoveryFingerprint: rt.childRecoveryFingerprint(*target), RecoveryLeafID: view.LeafID, RecoveryInputID: rt.active.input.InputID}
	if err := rt.manager.SaveInvocation(context.Background(), inv); err != nil {
		rt.releaseDelegation(scope.TraceID)
		return delegateStart{}, err
	}
	child := scope
	child.InvocationID, child.ParentInvocationID, child.TurnID, child.SelectionRevision = inv.ID, scope.InvocationID, "", 0
	start := delegateStart{inv: inv, def: *target, scope: child, task: args.Task, parent: rt.active.budget}
	// Delegation uses the parent's remaining activity budget (12 limits table).
	if activity := rt.active.activity; activity != nil {
		start.remaining = activity.remaining
	}
	return start, nil
}

// runChild runs outside the mailbox. The child has its own ledger view whose
// every commit is charged to the parent trace ledger first, so the shared
// trace total is enforced live, including across concurrent children. The
// child has no Boundary: it never creates parent Turns, consumes steering or
// writes parent history. It returns the child's logical model call count.
func (rt *runtime) runChild(ctx context.Context, start delegateStart) (string, int, error) {
	ledger := rt.newChildBudget(ctx, start)
	before := ledger.Snapshot().LogicalModelCalls
	childCtx := einorun.ChildContext(ctx, start.scope)
	// The child's compaction service is invocation-local: it only ever sees
	// this child's own task, responses and tool results.
	summaryModel, err := einorun.NewAuxiliaryModel(start.def.Model, &childSink{rt: rt}, rt.newChildBudget(ctx, start), start.scope, "compaction", "")
	if err != nil {
		return "", 0, err
	}
	compactor := &childCompactor{scope: start.scope, model: summaryModel, limit: start.parent.Limits().TraceCompactions}
	compactor.record(childUserMessage(start.scope, start.task))
	sink := &childSink{rt: rt, allowed: map[string]bool{}, compactor: compactor}
	for _, name := range start.def.Tools {
		sink.allowed[name] = true
	}
	deps := einorun.Deps{Model: start.def.Model, Sink: sink, Budget: ledger, Instruction: start.def.Instruction, Name: start.def.Name, Scope: start.scope, RemainingActivity: start.remaining, ContextBudget: contextBudget(start.def.Model), Compactor: compactor}
	if len(sink.allowed) != 0 {
		if err := rt.childTools(start, sink, &deps); err != nil {
			return "", 0, err
		}
	}
	ag, err := einorun.NewAgent(childCtx, deps)
	if err != nil {
		return "", 0, err
	}
	text, runErr := einorun.RunDelegated(childCtx, ag, start.task, llm.UsesObservedTransport(start.def.Model))
	return text, rt.manager.View().InvocationBudgets[start.inv.ID].Usage.LogicalModelCalls - before, runErr
}

// childCompactor is one delegated invocation's own compaction service. Its
// active summary projection is still process-local; model attempts, candidates
// and request reservations are private durable facts. Persisting the active
// projection and its recovery binding is handled separately.
//
// Ownership rules that make this safe:
//   - Material comes exclusively from the child's own conversation, collected
//     by childSink from the child's task, its complete assistant responses and
//     its committed tool observations. The parent branch is never read.
//   - Nothing is appended to state.View.Messages, no parent Turn is created and
//     no maintenance operation is accepted, so the parent projection and the
//     per-trace compaction limit stay untouched.
//   - Each summary request uses the same ValidatedModel attempt/transport path
//     and Eino compaction service, with an independent call ledger committed
//     jointly with the invocation and parent trace occupancy.
type childCompactor struct {
	mu    sync.Mutex
	scope agent.ExecutionScope
	model einomodel.AgenticModel
	// path is this child's own conversation in order, owned by this compactor.
	path []agent.AgentMessage
	// compactions bounds automatic compactions for one child invocation.
	compactions int
	limit       int
}

// record appends child-owned entries. Recording is the only way material enters
// the compactor, so no parent message can ever become summary material.
func (c *childCompactor) record(msgs ...agent.AgentMessage) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = append(c.path, msgs...)
}

// CompactForRequest implements agent.CompactionRequester for one child. It
// compacts only when the framework's visible input is exactly this child's
// current projection, so no in-flight child message can be dropped.
func (c *childCompactor) CompactForRequest(ctx context.Context, scope agent.ExecutionScope, req agent.CompactionRequest) ([]agent.AgentMessage, bool, error) {
	if c == nil || (req.Reason != reasonSoftThreshold && req.Reason != reasonOverflow) {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if scope != c.scope {
		return nil, false, product.NewError(product.CodeStateConflict, "child compaction scope is not active")
	}
	c.mu.Lock()
	if c.compactions >= c.limit {
		c.mu.Unlock()
		return nil, false, nil
	}
	path := append([]agent.AgentMessage(nil), c.path...)
	projected, err := agent.ConvertToLLM(agent.ProjectHistory(path))
	if err != nil || len(projected) != req.VisibleMessages {
		c.mu.Unlock()
		return nil, false, err // the child's own entries are not all recorded yet
	}
	part, err := agent.PartitionForCompaction(path, compactKeepEntries)
	if err != nil {
		c.mu.Unlock()
		return nil, false, nil // no_op: no model call and no summary entry
	}
	c.compactions++
	model := c.model
	c.mu.Unlock()
	previous := ""
	if part.Previous != nil {
		previous = part.Previous.Summary.Text
	}
	// A failed candidate keeps the old child projection (08 §6); only
	// cancellation stops the child's request.
	candidate, err := einorun.GenerateCompaction(ctx, model, previous, part.H, part.P, compactToolBytes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		return nil, false, nil
	}
	summary := agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindCompactionSummary, Status: agent.StatusComplete,
		Source:  agent.SourceRef{Kind: agent.SourceModel, Description: "compaction"},
		Scope:   agent.MessageScope{SessionID: c.scope.SessionID, TraceID: c.scope.TraceID, InvocationID: c.scope.InvocationID},
		Summary: &agent.SummaryMessage{Text: candidate.Text, FirstKeptID: part.FirstKeptID, TemplateVersion: candidate.TemplateVersion}}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = append(c.path, summary)
	return agent.ProjectHistory(append([]agent.AgentMessage(nil), c.path...)), true, nil
}

// childUserMessage is the child's own task input, the first entry it can
// summarize. It is not a parent input and carries no InputID.
func childUserMessage(scope agent.ExecutionScope, text string) agent.AgentMessage {
	return agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindUser, Status: agent.StatusComplete,
		Source: agent.SourceRef{Kind: agent.SourceDirectParent},
		Scope:  agent.MessageScope{SessionID: scope.SessionID, TraceID: scope.TraceID, InvocationID: scope.InvocationID},
		Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser,
			ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.UserInputText{Text: text})}}}
}

// childAssistantMessage copies one complete child response as summary material.
func childAssistantMessage(scope agent.ExecutionScope, msg *schema.AgenticMessage) agent.AgentMessage {
	standard := *msg
	standard.ContentBlocks = append([]*schema.ContentBlock(nil), msg.ContentBlocks...)
	return agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindAssistant, Status: agent.StatusComplete,
		Source:   agent.SourceRef{Kind: agent.SourceModel},
		Scope:    agent.MessageScope{SessionID: scope.SessionID, TraceID: scope.TraceID, InvocationID: scope.InvocationID},
		Standard: &standard}
}

// childToolResultMessage copies one child tool result the way the framework
// presents it, so the child's material matches its actual conversation.
func childToolResultMessage(scope agent.ExecutionScope, providerCallID, name, content string) agent.AgentMessage {
	result := &schema.FunctionToolResult{CallID: providerCallID, Name: name,
		Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: content}}}}
	return agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindToolResult, Status: agent.StatusComplete,
		Source: agent.SourceRef{Kind: agent.SourceTool},
		Scope:  agent.MessageScope{SessionID: scope.SessionID, TraceID: scope.TraceID, InvocationID: scope.InvocationID},
		Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser,
			ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(result)}}}
}

// boundText keeps at most limit bytes on a UTF-8 boundary.
func boundText(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], true
}

// failInterruptedInvocations runs at open: a running child from an earlier
// process is marked interrupted (non-terminal). It is never continued or
// re-run; the claimed parent call keeps the existing unknown-effect rules and
// the delegation evidence query below reports what the child durably did.
func failInterruptedInvocations(manager *state.Manager) error {
	for _, inv := range manager.View().Invocations {
		if inv.State != "running" {
			continue
		}
		inv.State = "interrupted"
		if err := manager.SaveInvocation(context.Background(), inv); err != nil {
			return err
		}
	}
	return nil
}

// childTools wraps the child's declared tools exactly like the parent's, but
// executes them through the child sink, so every call runs the controlled
// pipeline under the child invocation. Tool claims are charged to the trace
// ledger directly: its durable usage is the claim's compare-and-set base.
func (rt *runtime) childTools(start delegateStart, sink *childSink, deps *einorun.Deps) error {
	environment, workspace := rt.resourceDomain()
	exec, err := tools.NewExecutor(start.scope.Generation, rt.searchToolDefinitions(), sink, sessionAuthorizer{rt: rt, scope: start.scope}, start.parent,
		tools.WithCompiledSchemas(rt.opts.compiledTools), tools.WithOperations(rt.opts.Operations), tools.WithResourceScheduler(rt.resourceScheduler()), tools.WithResourceDomain(environment, workspace))
	if err != nil {
		return err
	}
	for _, name := range start.def.Tools {
		info := toolInfoFor(rt.opts.ToolInfos, name)
		if info == nil {
			return product.NewError(product.CodeResourceUnavailable, "child tool is unavailable")
		}
		kind := ""
		for _, def := range rt.opts.Tools {
			if def.Name == name {
				kind = def.ToolInterface
				break
			}
		}
		wrapped, err := einorun.NewPipelineToolForInterface(info, exec, start.scope, kind)
		if err != nil {
			return err
		}
		deps.Tools = append(deps.Tools, wrapped)
	}
	deps.UnknownToolsHandler = func(ctx context.Context, name, arguments string) (string, error) {
		out, err := exec.RejectUnavailable(ctx, einorun.ScopeFromContext(ctx, start.scope), compose.GetToolCallID(ctx), name, arguments)
		if err != nil {
			return "", err
		}
		raw, _ := json.Marshal(out)
		return string(raw), nil
	}
	return nil
}

// DelegateEvidenceQuery is the builtin, read-only reconciliation query for an
// unresolved delegate_task call. It inspects only durable invocation and child
// call records; it never runs a model or tool.
const DelegateEvidenceQuery = "delegate_task"

func (rt *runtime) delegateEvidence(ctx context.Context, request ReconcileQueryRequest) (ReconcileEvidence, error) {
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		v := rt.manager.View()
		call, ok := v.Calls[request.CallID]
		if !ok || call.Call.Name != delegateToolName || call.Scope.TraceID != request.TraceID {
			return nil, product.NewError(product.CodeUnsupportedCapability, "delegation evidence does not apply to this call")
		}
		evidence := ReconcileEvidence{EvidenceSource: "delegation-invocation"}
		var root *state.Invocation
		for _, inv := range v.Invocations {
			if inv.ParentCallID == request.CallID && inv.TraceID == request.TraceID {
				copy := inv
				root = &copy
				break
			}
		}
		// The child starts only after its running invocation is durable, so a
		// claimed task call without one never started a child.
		if root == nil {
			evidence.EvidenceRefs = []string{"delegation:" + request.CallID + ":no-invocation"}
			evidence.TrustedNoStart = true
			return evidence, nil
		}
		evidence.EvidenceRefs = []string{"invocation:" + root.ID}
		tree := invocationTree(v, root.ID)
		claimed := 0
		for _, inv := range tree {
			if inv.State == "running" {
				evidence.RemainingUnknown = append(evidence.RemainingUnknown, "delegated invocation "+inv.ID+" is still running")
			}
		}
		for _, c := range v.Calls {
			for _, inv := range tree {
				if c.Scope.InvocationID != inv.ID || c.Scope.TraceID != inv.TraceID {
					continue
				}
				if c.Claimed {
					claimed++
				}
				switch {
				case childCallUnknown(v, c):
					evidence.RemainingUnknown = append(evidence.RemainingUnknown, "delegated tool "+c.Call.CallID+" effect is unknown")
				case c.Claimed && childCallConfirmed(v, c):
					evidence.ConfirmedEffects = append(evidence.ConfirmedEffects, "tool:"+c.Call.CallID)
				}
			}
		}
		sort.Strings(evidence.RemainingUnknown)
		sort.Strings(evidence.ConfirmedEffects)
		switch {
		case len(evidence.RemainingUnknown) != 0:
		case root.State == "completed" || claimed != 0:
			// The saved child result stays in the invocation record.
			evidence.ConfirmedExecution = true
		default:
			// Interrupted, failed or cancelled before any child tool claim:
			// the delegation cannot have produced an external effect.
			evidence.TrustedNoStart = true
		}
		return evidence, nil
	})
	if err != nil {
		return ReconcileEvidence{}, err
	}
	return value.(ReconcileEvidence), nil
}

// delegateToolAllowed hides the task tool from callers without targets.
func (rt *runtime) delegateToolAllowed(traceID string) bool {
	tr := rt.manager.View().Traces[traceID]
	if tr == nil {
		return false
	}
	def, err := rt.definitionFor(tr.Target)
	return err == nil && rt.canDelegate(def.Name)
}
