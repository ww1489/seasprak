package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

// WorkflowModelBinding is the only model binding a compiled workflow target
// may name: the session's trusted model for the executing trace.
const WorkflowModelBinding = "default"

// CompileWorkflowTarget compiles def against this session's trusted bindings:
// the registered application tools (by name and argument schema), the
// session model as binding "default", and already registered workflow
// targets as fixed-version subflows. Product builtins that need a model turn
// (search_tools, delegate_task) are not bindable. The host sets Delegable on
// the returned definition when the workflow may be delegated to.
func CompileWorkflowTarget(def agent.WorkflowDefinition, opts Options) (agent.AgentDefinition, error) {
	bindings := agent.WorkflowBindings{Models: map[string]bool{WorkflowModelBinding: true}, Tools: map[string]json.RawMessage{}, Subflows: map[string]*agent.CompiledWorkflow{}}
	for _, t := range opts.Tools {
		if isDelegateBuiltin(t) || (t.Name == "search_tools" && t.Version == "search-tools-v1") {
			continue
		}
		bindings.Tools[t.Name] = append(json.RawMessage(nil), t.Schema...)
	}
	for _, target := range opts.Agents {
		if target.Kind == agent.AgentKindWorkflow && target.Workflow != nil {
			bindings.Subflows[target.Name+"@"+target.Version] = target.Workflow
		}
	}
	compiled, err := agent.CompileWorkflow(def, bindings)
	if err != nil {
		return agent.AgentDefinition{}, err
	}
	return agent.AgentDefinition{Name: def.Name, Version: def.Version, Description: def.Description, Kind: agent.AgentKindWorkflow, Workflow: compiled}, nil
}

// workflowInput extracts the workflow input object from accepted content:
// {"input": {...}}, or {"text": "<JSON object>"} as sent by text-only
// clients. It validates the full input schema and never asks a model.
func workflowInput(c *agent.CompiledWorkflow, content json.RawMessage) (map[string]any, error) {
	var body struct {
		Input json.RawMessage `json:"input"`
		Text  *string         `json:"text"`
	}
	if err := json.Unmarshal(content, &body); err != nil {
		return nil, product.NewError(product.CodeInvalidArgument, "workflow input must be a JSON object")
	}
	raw := []byte(body.Input)
	if len(raw) == 0 && body.Text != nil {
		raw = []byte(*body.Text)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, product.NewError(product.CodeInvalidArgument, "workflow input must be a JSON object")
	}
	if err := agent.ValidateWorkflowInput(c, raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, product.NewError(product.CodeInvalidArgument, "workflow input must be a JSON object")
	}
	return out, nil
}

func (rt *runtime) workflowTarget(target agent.TargetAgent) (agent.AgentDefinition, bool) {
	def, err := rt.definitionFor(target)
	return def, err == nil && def.Kind == agent.AgentKindWorkflow && def.Workflow != nil
}

// admitWorkflowInput runs in the mailbox before acceptance, so a rejected
// request writes nothing. Workflows do not take free text: directed inputs
// into a workflow trace are rejected rather than routed to a model.
func (rt *runtime) admitWorkflowInput(cmd agent.InputCommand, target agent.TargetAgent) error {
	v := rt.manager.View()
	noText := product.NewError(product.CodeUnsupportedCapability, "workflow does not accept free-text input")
	switch cmd.Kind {
	case "steering", "follow_up":
		if tr := v.Traces[cmd.TargetTraceID]; tr != nil {
			if _, ok := rt.workflowTarget(tr.Target); ok {
				return noText
			}
		}
		return nil
	case "chat":
		if active := v.Traces[v.ActiveTrace]; active != nil && active.State == "running" && (cmd.TargetAgent == "" || cmd.TargetAgent == active.Target.Name) {
			if _, ok := rt.workflowTarget(active.Target); ok {
				return noText
			}
			return nil
		}
	case "prompt":
	default:
		return nil
	}
	def, ok := rt.workflowTarget(target)
	if !ok {
		return nil
	}
	_, err := workflowInput(def.Workflow, cmd.Content)
	return err
}

// executeWorkflow runs a standalone workflow trace without the main model
// loop: it consumes the input, runs the compiled graph through session node
// execution and appends one workflow_result message. Completed nodes from an
// earlier segment of the same invocation are reused, never executed again.
func (rt *runtime) executeWorkflow(frame *execution, inputID string, def agent.AgentDefinition) error {
	ctx := frame.ctx
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		if rt.active != frame {
			return nil, product.NewError(product.CodeStateConflict, "execution is no longer active")
		}
		in := rt.manager.View().Inputs[inputID]
		if in == nil || in.TraceID != frame.scope.TraceID {
			return nil, product.NewError(product.CodeInternal, "execution input is missing")
		}
		// Consume is idempotent for an already consumed input (resume).
		if err := rt.manager.Consume(ctx, inputID); err != nil {
			return nil, err
		}
		return in.Content, nil
	})
	if err != nil {
		return err
	}
	input, err := workflowInput(def.Workflow, value.(json.RawMessage))
	if err != nil {
		return err
	}
	run, err := rt.newWorkflowRun(frame.scope, frame.budget, frame.currentModel)
	if err != nil {
		return err
	}
	run.stop = &workflowStop{}
	out, err := run.invoke(ctx, def.Workflow, input)
	if err != nil {
		if stopped, _ := run.stop.state(); stopped && ctx.Err() == nil {
			// Stopped at a node boundary (Pause or approval wait). The
			// coordinator commits the stop after this segment has exited.
			frame.workflowStop = run.stop
			return nil
		}
		return err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return product.NewError(product.CodeInternal, "workflow result is not valid JSON")
	}
	return rt.do(context.WithoutCancel(ctx), func(rt *runtime) error {
		if rt.active != frame {
			return product.NewError(product.CodeStateConflict, "execution is no longer active")
		}
		if err := frame.ctx.Err(); err != nil {
			return err
		}
		msg := agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindCustom, Status: agent.StatusComplete,
			Scope:  agent.MessageScope{SessionID: frame.scope.SessionID, TraceID: frame.scope.TraceID, InvocationID: frame.scope.InvocationID},
			Source: agent.SourceRef{Kind: agent.SourceTool, Description: "workflow"},
			Custom: &agent.CustomMessage{CustomType: "workflow_result", Content: schema.UserAgenticMessage(string(raw)), Details: raw, Display: true}}
		return rt.manager.AppendMessage(context.Background(), msg)
	})
}

// workflowRun is the trusted agent.WorkflowNodeExecutor of one workflow
// invocation. Node records are committed in the session mailbox; tool nodes
// go through the controlled executor with origin workflow_node.
type workflowRun struct {
	rt     *runtime
	scope  agent.ExecutionScope // TurnID and SelectionRevision are always empty
	exec   *tools.Executor
	budget *agent.BudgetLedger
	model  model.AgenticModel
	prefix string // node path prefix inside subflows
	calls  *atomic.Int64
	// stop is set only for a standalone workflow trace; delegated child
	// workflows neither pause nor wait for approvals.
	stop *workflowStop
	// inflight joins node executions still running in parallel branches, so
	// a segment never ends while one of its nodes is executing.
	inflight *workflowGate
}

// workflowGate admits node starts in the mailbox until the invocation ends.
// closed is mailbox-owned; wg counts admitted nodes still executing.
type workflowGate struct {
	closed bool
	wg     sync.WaitGroup
}

// errWorkflowStopped ends a workflow segment at a node boundary. It is never
// a node or trace failure: the coordinator commits the stop after exit.
var errWorkflowStopped = errors.New("workflow segment stopped at a node boundary")

// workflowStop records why a standalone workflow segment stopped: a Pause
// request, or a tool node waiting for a runtime approval.
type workflowStop struct {
	mu      sync.Mutex
	stopped bool
	waiting string // nodeExecutionId of the tool node waiting for approval
}

func (s *workflowStop) set(waiting string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if waiting != "" && s.waiting == "" {
		s.waiting = waiting
	}
}

func (s *workflowStop) state() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped, s.waiting
}

// boundary runs in the mailbox before a model or tool node starts. Once the
// segment stops (Pause accepted or another node waits), no further node starts.
func (w *workflowRun) boundary(rt *runtime) error {
	if w.inflight.closed {
		return product.NewError(product.CodeStateConflict, "workflow invocation has ended")
	}
	if w.stop == nil {
		return nil
	}
	if stopped, _ := w.stop.state(); stopped || rt.active.pauseID != "" {
		w.stop.set("")
		return errWorkflowStopped
	}
	return nil
}

// admit is the mailbox-side start of a node that will execute: it passes the
// boundary and is counted until done is called.
func (w *workflowRun) admit(rt *runtime) error {
	if err := w.boundary(rt); err != nil {
		return err
	}
	w.inflight.wg.Add(1)
	return nil
}

func (rt *runtime) newWorkflowRun(scope agent.ExecutionScope, budget *agent.BudgetLedger, m model.AgenticModel) (*workflowRun, error) {
	scope.TurnID, scope.SelectionRevision = "", 0
	environment, workspace := rt.resourceDomain()
	exec, err := tools.NewExecutor(scope.Generation, rt.searchToolDefinitions(), rt, sessionAuthorizer{rt: rt, scope: scope}, budget,
		tools.WithCompiledSchemas(rt.opts.compiledTools), tools.WithOperations(rt.opts.Operations), tools.WithResourceScheduler(rt.resourceScheduler()), tools.WithResourceDomain(environment, workspace))
	if err != nil {
		return nil, err
	}
	return &workflowRun{rt: rt, scope: scope, exec: exec, budget: budget, model: m, calls: new(atomic.Int64), inflight: &workflowGate{}}, nil
}

func (w *workflowRun) invoke(ctx context.Context, c *agent.CompiledWorkflow, input map[string]any) (map[string]any, error) {
	// Tools and the authorizer resolve the node scope from ctx, never an
	// enclosing parent tool scope.
	ctx = einorun.WithExecutionScope(ctx, w.scope)
	graph, err := einorun.BuildWorkflowGraph(ctx, c, w, nil)
	if err != nil {
		return nil, err
	}
	out, err := graph.Invoke(ctx, input)
	if w.prefix == "" {
		// A failed or stopped branch may return before a sibling node ends:
		// admit no further node, then join the admitted ones.
		closeErr := w.rt.do(context.WithoutCancel(ctx), func(*runtime) error { w.inflight.closed = true; return nil })
		w.inflight.wg.Wait()
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}
	return out, err
}

// nodeExecutionID is stable across retries and resume: invocation, node path
// and the logical visit ordinal (always 1 until loops exist).
func (w *workflowRun) nodeExecutionID(nodeID string) string {
	return w.scope.InvocationID + ":" + w.prefix + nodeID + ":1"
}

type workflowStep struct {
	node   state.WorkflowNodeRun
	done   bool
	result string
}

// active reports whether this run's scope is still the executing one.
func (w *workflowRun) active(rt *runtime) error {
	if err := rt.writable(); err != nil {
		return err
	}
	if !rt.matchesToolExecution(w.scope) {
		return product.NewError(product.CodeStateConflict, "workflow execution is no longer active")
	}
	return nil
}

// savedStep returns a terminal node's saved outcome.
func savedStep(node state.WorkflowNodeRun) (workflowStep, error) {
	if node.State == "completed" {
		return workflowStep{node: node, done: true, result: node.Result}, nil
	}
	return workflowStep{}, product.Errorf(product.CodeStateConflict, "workflow node %q already failed", node.NodeID)
}

func (w *workflowRun) RunModel(ctx context.Context, nodeID, _ string, prompt string) (string, error) {
	id := w.nodeExecutionID(nodeID)
	value, err := w.rt.call(context.WithoutCancel(ctx), func(rt *runtime) (any, error) {
		if err := w.active(rt); err != nil {
			return nil, err
		}
		if node, ok := rt.manager.View().WorkflowNodes[id]; ok {
			if node.State != "accepted" {
				return savedStep(node)
			}
			if node.Kind != "model" {
				return nil, product.NewError(product.CodeStateConflict, "workflow node kind changed")
			}
			// Accepted earlier and interrupted: run again.
			return workflowStep{node: node}, w.admit(rt)
		}
		if err := w.admit(rt); err != nil {
			return nil, err
		}
		node := state.WorkflowNodeRun{ID: id, TraceID: w.scope.TraceID, InvocationID: w.scope.InvocationID, NodeID: w.prefix + nodeID, Kind: "model", State: "accepted"}
		if err := rt.manager.BeginWorkflowNode(context.Background(), node, nil); err != nil {
			w.inflight.wg.Done()
			return nil, err
		}
		return workflowStep{node: node}, nil
	})
	if err != nil {
		return "", err
	}
	step := value.(workflowStep)
	if step.done {
		return step.result, nil
	}
	defer w.inflight.wg.Done()
	text, err := w.callModel(ctx, id, prompt)
	if err != nil {
		if ctx.Err() != nil {
			return "", err // interrupted: the node stays accepted for resume
		}
		return "", errors.Join(err, w.finish(step.node, "failed", "", workflowErrorText(err), 1))
	}
	if err := w.finish(step.node, "completed", text, "", 1); err != nil {
		return "", err
	}
	return text, nil
}

// callModel charges the shared trace ledger before calling the trusted
// model. It creates no Turn, model attempt or provider call identity.
func (w *workflowRun) callModel(ctx context.Context, id, prompt string) (string, error) {
	if w.model == nil {
		return "", product.NewError(product.CodeResourceUnavailable, "model instance is unavailable")
	}
	observed := llm.UsesObservedTransport(w.model)
	transport := 1
	if observed {
		transport = 0 // each physical request is charged by the transport observer
	}
	if err := w.budget.ChargeDelegated(1, transport); err != nil {
		return "", err
	}
	w.calls.Add(1)
	if observed {
		ctx = llm.WithRequestObservation(ctx, llm.RequestIdentity{ModelCallID: id, AttemptID: id + ":" + agent.MustID(), Purpose: "workflow_node"}, workflowTransport{budget: w.budget})
	}
	msg, err := w.model.Generate(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage(prompt)})
	if err != nil {
		return "", err
	}
	if msg == nil {
		return "", product.NewError(product.CodeResourceUnavailable, "model returned no message")
	}
	var b strings.Builder
	for _, block := range msg.ContentBlocks {
		if block != nil && block.AssistantGenText != nil {
			b.WriteString(block.AssistantGenText.Text)
		}
	}
	return b.String(), nil
}

// workflowTransport charges each physical model request of a workflow node
// to the shared trace total without starting a Turn.
type workflowTransport struct{ budget *agent.BudgetLedger }

func (t workflowTransport) BeforeRequest(context.Context, llm.TransportRequest) error {
	return t.budget.ChargeDelegated(0, 1)
}

func (w *workflowRun) RunTool(ctx context.Context, nodeID, name string, args json.RawMessage) (string, error) {
	id := w.nodeExecutionID(nodeID)
	arguments := string(args)
	value, err := w.rt.call(context.WithoutCancel(ctx), func(rt *runtime) (any, error) {
		if err := w.active(rt); err != nil {
			return nil, err
		}
		v := rt.manager.View()
		node, exists := v.WorkflowNodes[id]
		if !exists {
			if err := w.admit(rt); err != nil {
				return nil, err
			}
			node = state.WorkflowNodeRun{ID: id, TraceID: w.scope.TraceID, InvocationID: w.scope.InvocationID, NodeID: w.prefix + nodeID, Kind: "tool", State: "accepted", ToolCallID: id, Attempt: 1}
			call := rt.workflowToolCall(w.scope, id, name, arguments)
			if err := rt.manager.BeginWorkflowNode(context.Background(), node, &call); err != nil {
				w.inflight.wg.Done()
				return nil, err
			}
			return workflowStep{node: node}, nil
		}
		if node.State != "accepted" {
			return savedStep(node)
		}
		if node.Kind != "tool" {
			return nil, product.NewError(product.CodeStateConflict, "workflow node kind changed")
		}
		if err := w.admit(rt); err != nil {
			return nil, err
		}
		call := v.Calls[node.ToolCallID]
		switch {
		case call.Claimed, call.Observation != nil && !state.WorkflowToolRetryable(call):
			// The executor returns the saved outcome or reports the conflict;
			// a claimed call is never started again.
			return workflowStep{node: node}, nil
		case call.Observation == nil && call.Scope.ExecutionID == w.scope.ExecutionID:
			return workflowStep{node: node}, nil
		case call.Observation == nil && node.ApprovalWait && w.stop != nil && rt.active.workflowFrom != "" && node.ToolCallID == call.Call.CallID:
			// The same call waited for approval in an earlier segment. It runs
			// with its original identity; policy asks again unless a live
			// answer of this process instance allows it once.
			return workflowStep{node: node}, nil
		case call.Observation == nil:
			// Registered by an earlier segment and never claimed, so it never
			// started. Close it before the next attempt.
			call.Observation = &agent.ToolObservation{Status: "skipped", Content: "workflow execution stopped before the tool started", SideEffect: "none"}
			if err := rt.manager.SaveCall(context.Background(), call); err != nil {
				w.inflight.wg.Done()
				return nil, err
			}
		}
		node.Attempt++
		node.ToolCallID, node.ApprovalWait = id+"#"+strconv.Itoa(node.Attempt), false
		next := rt.workflowToolCall(w.scope, node.ToolCallID, name, arguments)
		if err := rt.manager.BeginWorkflowNode(context.Background(), node, &next); err != nil {
			w.inflight.wg.Done()
			return nil, err
		}
		return workflowStep{node: node}, nil
	})
	if err != nil {
		return "", err
	}
	step := value.(workflowStep)
	if step.done {
		return step.result, nil
	}
	defer w.inflight.wg.Done()
	out, runErr := w.exec.RunWorkflowNode(einorun.WithExecutionScope(ctx, w.scope), w.scope, id, step.node.ToolCallID, name, arguments)
	if ctx.Err() != nil && (runErr != nil || out.Status == "cancelled") {
		return "", errors.Join(runErr, ctx.Err()) // interrupted: resume decides from the call record
	}
	var wait *agent.ApprovalWait
	if runErr != nil && w.stop != nil && errors.As(runErr, &wait) {
		// The call stays registered and unclaimed; the node becomes waiting
		// when the coordinator commits the stopped segment.
		w.stop.set(id)
		return "", errWorkflowStopped
	}
	if runErr != nil {
		// Some rejections (for example an approval request) return before an
		// observation exists. An unclaimed call never started: record that.
		closeErr := w.rt.do(context.Background(), func(rt *runtime) error {
			call, ok := rt.manager.View().Calls[step.node.ToolCallID]
			if !ok || call.Claimed || call.Observation != nil {
				return nil
			}
			call.Observation = &agent.ToolObservation{Status: "denied", Content: workflowErrorText(runErr), SideEffect: "none"}
			return rt.manager.SaveCall(context.Background(), call)
		})
		return "", errors.Join(runErr, closeErr, w.finish(step.node, "failed", "", workflowErrorText(runErr), 0))
	}
	if out.Status != "succeeded" {
		code := product.CodeResourceUnavailable
		if out.Status == "denied" {
			code = product.CodePermissionDenied
		}
		failure := product.Errorf(code, "workflow tool node %q %s", nodeID, out.Status)
		return "", errors.Join(failure, w.finish(step.node, "failed", "", failure.Error(), 0))
	}
	if err := w.finish(step.node, "completed", out.Content, "", 0); err != nil {
		return "", err
	}
	return out.Content, nil
}

// RunSubflow runs a bound fixed-version subflow inline in this invocation.
// Its nodes get their own node paths, so their identities stay distinct.
func (w *workflowRun) RunSubflow(ctx context.Context, nodeID string, sub *agent.CompiledWorkflow, input json.RawMessage) (map[string]any, error) {
	child := *w
	child.prefix = w.prefix + nodeID + "/"
	envelope, err := json.Marshal(map[string]json.RawMessage{"input": input})
	if err != nil {
		return nil, product.NewError(product.CodeInvalidArgument, "workflow subflow input is not valid JSON")
	}
	in, err := workflowInput(sub, envelope)
	if err != nil {
		return nil, err
	}
	return child.invoke(ctx, sub, in)
}

func (w *workflowRun) finish(node state.WorkflowNodeRun, next, result, message string, modelCalls int) error {
	node.State, node.Result, node.Error, node.ModelCalls = next, result, message, modelCalls
	return w.rt.do(context.Background(), func(rt *runtime) error {
		return rt.manager.FinishWorkflowNode(context.Background(), node)
	})
}

func workflowErrorText(err error) string {
	if pe, ok := product.AsError(err); ok {
		return pe.Code + ": " + pe.Message
	}
	return product.CodeInternal
}

// workflowToolCall builds the product call registered for a tool node. It has
// no provider call, Turn or selection revision: the definition binds it.
func (rt *runtime) workflowToolCall(scope agent.ExecutionScope, callID, name, arguments string) agent.ToolRecord {
	version := ""
	for _, def := range rt.opts.Tools {
		if def.Name == name {
			version = def.Version
			break
		}
	}
	sum := sha256.Sum256([]byte(name + "\n" + version + "\n" + arguments + "\n" + scope.Generation))
	return agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: callID, Name: name, Arguments: arguments, Generation: scope.Generation, Hash: hex.EncodeToString(sum[:])}}
}

// standaloneWorkflowScope reports whether scope is the root invocation of the
// active standalone workflow segment, the only workflow scope with a node
// boundary at which it can stop for an approval.
func (rt *runtime) standaloneWorkflowScope(scope agent.ExecutionScope) bool {
	if rt.active == nil || !rt.matchesExecution(scope) || scope.InvocationID != rt.active.scope.InvocationID {
		return false
	}
	tr := rt.manager.View().Traces[rt.active.scope.TraceID]
	if tr == nil {
		return false
	}
	_, workflow := rt.workflowTarget(tr.Target)
	return workflow
}

// commitWorkflowStop runs in the mailbox after the stopped workflow segment
// exited. It commits the paused trace (and waiting node and completed pause)
// and binds runtime questions of the waiting call to this stopped segment.
func (rt *runtime) commitWorkflowStop(frame *execution) error {
	_, waiting := frame.workflowStop.state()
	if err := rt.manager.CommitWorkflowStop(context.Background(), frame.scope.TraceID, frame.scope.ExecutionID, frame.pauseID, waiting); err != nil {
		return err
	}
	if waiting != "" {
		callID := rt.manager.View().WorkflowNodes[waiting].ToolCallID
		for _, pending := range rt.approvals {
			if pending.approval.CallID == callID && pending.decision == "" && pending.claimedExecution == "" {
				pending.workflowStop = frame.scope.ExecutionID
			}
		}
	}
	frame.cancel()
	frame.toolChunks = nil
	rt.active = nil
	close(frame.done)
	return nil
}

// closeWorkflowCalls closes workflow node calls of a stopped or terminal
// trace that were registered but never claimed: they provably never started.
// A claimed call without an observation is left to the unknown-effect rules.
func (rt *runtime) closeWorkflowCalls(ctx context.Context, traceID string) error {
	v := rt.manager.View()
	for id, call := range v.Calls {
		if call.Scope.TraceID != traceID || call.Claimed || call.Observation != nil {
			continue
		}
		if _, workflow := v.WorkflowNodeForCall(id); !workflow {
			continue
		}
		call.Observation = &agent.ToolObservation{Status: "skipped", Content: "workflow execution ended before the tool started", SideEffect: "none"}
		if err := rt.manager.SaveCall(ctx, call); err != nil {
			return err
		}
	}
	return nil
}

// acceptedCall is the acceptance rule for a tool call: a workflow call must
// belong to its registered node; every other call keeps the model rule.
func (rt *runtime) acceptedCall(v state.View, call agent.ToolRecord) bool {
	if node, ok := v.WorkflowNodeForCall(call.Call.CallID); ok {
		// Only the current attempt of a still accepted node may proceed.
		return node.State == "accepted" && node.ToolCallID == call.Call.CallID && node.TraceID == call.Scope.TraceID && node.InvocationID == call.Scope.InvocationID && call.Scope.TurnID == "" && call.Scope.SelectionRevision == 0 && call.Call.ProviderCallID == "" && call.Call.OperationID == ""
	}
	return acceptedAttemptForCall(v, call)
}

// matchesWorkflowChild accepts the scope of a delegated workflow invocation
// running inside the active execution. It is used only by the controlled
// tool pipeline ports; child scopes never create Turns or model facts.
func (rt *runtime) matchesWorkflowChild(scope agent.ExecutionScope) bool {
	if rt.active == nil || scope.ExecutionID == "" || scope.InvocationID == "" {
		return false
	}
	inv, ok := rt.manager.View().Invocations[scope.InvocationID]
	if !ok || inv.State != "running" || inv.TraceID != rt.active.scope.TraceID || inv.ParentInvocationID != rt.active.scope.InvocationID || scope.ParentInvocationID != inv.ParentInvocationID {
		return false
	}
	if _, workflow := rt.workflowTarget(inv.Target); !workflow {
		return false
	}
	current := rt.active.scope
	current.InvocationID, current.ParentInvocationID = scope.InvocationID, scope.ParentInvocationID
	current.TurnID, scope.TurnID = "", ""
	current.SelectionRevision, scope.SelectionRevision = 0, 0
	return current == scope
}

// LookupWorkflowTool implements agent.WorkflowToolSource for the executor.
func (rt *runtime) LookupWorkflowTool(ctx context.Context, scope agent.ExecutionScope, callID string) (agent.ToolRecord, error) {
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		if !rt.matchesToolExecution(scope) {
			return nil, product.NewError(product.CodeStateConflict, "workflow execution is not active")
		}
		v := rt.manager.View()
		call, ok := v.Calls[callID]
		if _, workflow := v.WorkflowNodeForCall(callID); !ok || !workflow || !rt.acceptedCall(v, call) || call.Scope.TraceID != scope.TraceID || call.Scope.InvocationID != scope.InvocationID {
			return nil, product.NewError(product.CodePermissionDenied, "workflow node call was not accepted")
		}
		if v.ReconciliationUnresolved(callID) {
			return nil, product.NewError(product.CodeReconciliationRequired, "tool result has conflicting evidence")
		}
		if effective, ok := v.EffectiveObservation(callID); ok {
			observation := effective.Observation
			call.Observation = &observation
		}
		return call, nil
	})
	if err != nil {
		return agent.ToolRecord{}, err
	}
	return value.(agent.ToolRecord), nil
}

// runChildWorkflow runs a delegated workflow under the child invocation. Its
// tool claims and model nodes are charged to the parent trace ledger.
func (rt *runtime) runChildWorkflow(ctx context.Context, start delegateStart) (string, int, error) {
	run, err := rt.newWorkflowRun(start.scope, start.parent, start.def.Model)
	if err != nil {
		return "", 0, err
	}
	out, err := run.invoke(ctx, start.def.Workflow, start.input)
	calls := int(run.calls.Load())
	if err != nil {
		return "", calls, err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", calls, product.NewError(product.CodeInternal, "workflow result is not valid JSON")
	}
	return string(raw), calls, nil
}

// validateWorkflowResume checks a paused workflow trace. Workflows have no
// runner checkpoint; the exited execution, the unchanged saved target and
// resolved effects are the preconditions for re-running from node records.
func (rt *runtime) validateWorkflowResume(traceID string, view state.View) error {
	tr := view.Traces[traceID]
	if tr == nil {
		return product.NewError(product.CodeNotFound, "trace not found")
	}
	if rt.active != nil || tr.State != "paused" || !tr.Started || tr.Settled || !tr.ExecutionStopped || view.ActiveTrace != traceID {
		return incompatibleResume("trace has no safely paused execution")
	}
	if view.HasUnresolvedEffects() {
		return product.NewError(product.CodeReconciliationRequired, "unknown tool effects block resume")
	}
	if _, ok := rt.workflowTarget(tr.Target); !ok || tr.Generation != rt.generation {
		return incompatibleResume("saved workflow target is not registered in this build")
	}
	return nil
}

// resumeWorkflow accepts Resume of a paused workflow trace and re-runs its
// graph in a new execution segment of the same invocation.
func (rt *runtime) resumeWorkflow(ctx context.Context, operation state.OperationCommand, view state.View) (state.OperationReceipt, error) {
	if err := rt.validateWorkflowResume(operation.Target, view); err != nil {
		return state.OperationReceipt{}, err
	}
	tr := view.Traces[operation.Target]
	inputID := ""
	for _, id := range view.Independent {
		if in := view.Inputs[id]; in.TraceID == tr.ID && in.State == "consumed" {
			inputID = id
			break
		}
	}
	if inputID == "" {
		return state.OperationReceipt{}, incompatibleResume("workflow original input is unavailable")
	}
	selected, err := rt.modelForTrace(tr)
	if err != nil {
		return state.OperationReceipt{}, err
	}
	executionID := agent.MustID()
	receipt, err := rt.manager.CommitWorkflowResume(ctx, operation, executionID)
	if err != nil {
		return state.OperationReceipt{}, err
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	frame := &execution{scope: agent.ExecutionScope{SessionID: rt.opts.SessionID, BranchID: view.BranchID, TraceID: tr.ID, InvocationID: tr.InvocationID, ExecutionID: executionID, Generation: tr.Generation},
		ctx: workerCtx, cancel: cancel, done: make(chan struct{}), budget: agent.NewBudget(tr.Limits), resumeID: receipt.OperationID, currentModel: selected, workflowFrom: tr.ExecutionID}
	frame.budget.Restore(tr.Usage)
	rt.setBudgetPersistence(frame)
	rt.active = frame
	go rt.runSegment(frame, inputID)
	return receipt, nil
}
