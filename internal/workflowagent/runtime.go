package workflowagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/storage"
)

var errStopped = errors.New("workflow stopped at a node boundary")

func canonicalInput(raw json.RawMessage) (json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value map[string]any
	if d.Decode(&value) != nil || value == nil {
		return nil, product.NewError(product.CodeInvalidArgument, "workflow input must be one JSON object")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, product.NewError(product.CodeInvalidArgument, "workflow input has trailing content")
	}
	out, err := json.Marshal(value)
	if err != nil {
		return nil, product.NewError(product.CodeInvalidArgument, "workflow input is invalid")
	}
	return out, nil
}
func (w *WorkflowAgent) principal(value string) (string, error) {
	if value == "" {
		value = w.opts.Principal
	}
	if value != w.state.Initial.Principal {
		return "", product.NewError(product.CodePermissionDenied, "principal does not own this workflow run")
	}
	return value, nil
}
func (w *WorkflowAgent) writableLocked() error {
	if w.opts.ReadOnly {
		return product.NewError(product.CodePermissionDenied, "workflow is read-only")
	}
	if w.closed || w.closing {
		return product.NewError(product.CodeStateConflict, "workflow is closing or closed")
	}
	if w.broken != nil {
		return w.broken
	}
	return nil
}
func revision(expected *uint64, actual uint64) error {
	if expected != nil && *expected != actual {
		return product.NewError(product.CodeStateConflict, "workflow revision changed")
	}
	return nil
}

func (w *WorkflowAgent) SubmitInput(ctx context.Context, cmd WorkflowInputCommand) (WorkflowInputReceipt, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowInputReceipt{}, err
	}
	raw, err := canonicalInput(cmd.Input)
	if err != nil {
		return WorkflowInputReceipt{}, err
	}
	hash := rawHash(raw)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.writableLocked(); err != nil {
		return WorkflowInputReceipt{}, err
	}
	principal, err := w.principal(cmd.Principal)
	if err != nil {
		return WorkflowInputReceipt{}, err
	}
	if previous := w.state.Input; previous != nil && cmd.IdempotencyKey != "" && previous.Key == cmd.IdempotencyKey && previous.Principal == principal {
		if previous.Digest != hash {
			return WorkflowInputReceipt{}, product.NewError(product.CodeIdempotencyConflict, "input key belongs to different content")
		}
		return previous.Receipt, nil
	}
	if err := revision(cmd.ExpectedRevision, w.state.Revision); err != nil {
		return WorkflowInputReceipt{}, err
	}
	if w.state.Run.State != "created" || w.state.Input != nil {
		return WorkflowInputReceipt{}, product.NewError(product.CodeStateConflict, "workflow accepts only one input")
	}
	if err := ValidateWorkflowInput(w.compiled, raw); err != nil {
		return WorkflowInputReceipt{}, err
	}
	receipt := WorkflowInputReceipt{RunID: w.opts.RunID, InputID: agent.MustID(), DefinitionVersion: w.state.Initial.Manifest.Definition.Version, BindingVersion: w.binding, State: "accepted", AcceptedCommit: w.state.Revision + 1}
	input := inputRecord{Receipt: receipt, Input: raw, Principal: principal, Key: cmd.IdempotencyKey, Digest: hash}
	run := runRecord{State: "running", ExecutionID: agent.MustID(), InvocationID: agent.MustID()}
	if err := w.commitLocked(ctx, []storage.Record{record("workflow_input", receipt.InputID, input), record("workflow_run", w.opts.RunID, run)}, []agent.Event{w.event("workflow.input.accepted", "", receipt), w.event("workflow.state_changed", "", map[string]any{"state": "running"})}); err != nil {
		return WorkflowInputReceipt{}, err
	}
	w.startLocked("")
	return receipt, nil
}
func (w *WorkflowAgent) startLocked(from string) {
	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), w.state.Initial.Limits.ActivityBudget-w.state.Run.ActivityUsed)
	frame := &segment{scope: agent.ExecutionScope{WorkflowRunID: w.opts.RunID, WorkflowDefinitionHash: w.compiled.Hash, InvocationID: w.state.Run.InvocationID, ExecutionID: w.state.Run.ExecutionID, Generation: w.binding}, ctx: ctx, cancel: cancel, done: make(chan struct{}), startedAt: startedAt, semaphore: make(chan struct{}, config.SubagentConcurrency), from: from}
	w.active = frame
	go w.execute(frame)
}
func (w *WorkflowAgent) execute(frame *segment) {
	runner := &workflowRun{owner: w, frame: frame, scope: frame.scope}
	var in map[string]any
	w.mu.Lock()
	raw := append(json.RawMessage(nil), w.state.Input.Input...)
	w.mu.Unlock()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	err := d.Decode(&in)
	var out map[string]any
	if err == nil {
		out, err = runner.invoke(frame.ctx, w.compiled, in)
	}
	// Eino may return one failing branch before a sibling exits. Close admission
	// under the same mutex as Add, then join every admitted node without it.
	w.mu.Lock()
	frame.gateClosed = true
	w.mu.Unlock()
	frame.nodes.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active != frame {
		return
	}
	run := w.state.Run
	records := []storage.Record{}
	state := run.State
	var failed NodeRun
	for _, n := range w.state.Nodes {
		if n.State == "failed" && (failed.ID == "" || n.ErrorCode == errorCode(err)) {
			failed = n
			if n.ErrorCode == errorCode(err) {
				break
			}
		}
	}
	switch {
	case state == "cancelling":
		run.State = "cancelled"
	case failed.ID != "":
		// A joined sibling's durable failure cannot be hidden by another
		// branch asking to pause for approval.
		run.State, run.ErrorCode, run.FailedNode = "failed", failed.ErrorCode, failed.ID
	case state == "pausing" || errors.Is(err, errStopped) || frame.ctx.Err() != nil:
		run.State = "paused"
	case err != nil:
		run.State = "failed"
		run.ErrorCode = errorCode(err)
	default:
		run.State = "completed"
		run.Result, _ = json.Marshal(out)
	}
	run.ExecutionStopped = true
	run.ActivityUsed += time.Since(frame.startedAt)
	if run.State == "paused" {
		for _, pending := range w.approvals {
			if pending.decision == "" && pending.requestedExecution == frame.scope.ExecutionID {
				n := w.state.Nodes[pending.view.NodeExecutionID]
				if n.State == "accepted" {
					n.State = "waiting"
					records = append(records, nodeFact(n))
				}
				pending.stoppedExecution = frame.scope.ExecutionID
			}
		}
	}
	if terminal(run.State) {
		records = append(records, w.closeUnclaimedLocked()...)
	}
	records = append(records, record("workflow_run", w.opts.RunID, run))
	for _, op := range w.state.Operations {
		if op.State == "running" {
			op.State = "completed"
			records = append(records, record("workflow_operation", op.Receipt.OperationID, op))
		}
	}
	err = w.commitLocked(context.Background(), records, []agent.Event{w.event("workflow.state_changed", "", map[string]any{"state": run.State, "errorCode": run.ErrorCode, "failedNode": run.FailedNode})})
	if err == nil {
		w.clearTransientLocked()
		for _, p := range w.approvals {
			if terminal(run.State) && p.decision == "" {
				p.view.State = "cancelled"
			} else if p.stoppedExecution == frame.scope.ExecutionID && p.decision == "" {
				p.view.State = "ready"
			}
		}
	}
	frame.cancel()
	w.active = nil
	close(frame.done)
}
func (w *WorkflowAgent) closeUnclaimedLocked() []storage.Record {
	var out []storage.Record
	for id, c := range w.state.Calls {
		if !c.Claimed && c.Observation == nil {
			c.Observation = &agent.ToolObservation{Status: "skipped", SideEffect: "none", Content: "workflow stopped before the tool started"}
			out = append(out, record("workflow_tool_observation", id, c))
		}
	}
	return out
}

type workflowRun struct {
	owner  *WorkflowAgent
	frame  *segment
	scope  agent.ExecutionScope
	prefix string
	depth  int
}

func (r *workflowRun) invoke(ctx context.Context, c *CompiledWorkflow, in map[string]any) (map[string]any, error) {
	graph, err := BuildWorkflowGraph(ctx, c, r, nil)
	if err != nil {
		return nil, err
	}
	return graph.Invoke(einorun.WithExecutionScope(ctx, r.scope), in)
}
func (r *workflowRun) nodeID(path string) string {
	return r.scope.WorkflowRunID + ":" + r.scope.InvocationID + ":" + path + ":1"
}
func (r *workflowRun) admit(ctx context.Context, nodeID, kind, name, args string) (NodeRun, bool, error) {
	if kind != "subflow" {
		select {
		case r.frame.semaphore <- struct{}{}:
		case <-ctx.Done():
			return NodeRun{}, false, ctx.Err()
		}
	}
	admitted := false
	defer func() {
		if kind != "subflow" && !admitted {
			<-r.frame.semaphore
		}
	}()
	w := r.owner
	w.mu.Lock()
	defer w.mu.Unlock()
	id := r.nodeID(r.prefix + nodeID)
	if n, exists := w.state.Nodes[id]; exists && n.State == "completed" {
		return n, true, nil
	}
	if w.active == r.frame && w.state.Run.State == "pausing" {
		return NodeRun{}, false, errStopped
	}
	if w.active != r.frame || r.frame.gateClosed {
		return NodeRun{}, false, product.NewError(product.CodeStateConflict, "workflow segment has ended")
	}
	if err := ctx.Err(); err != nil {
		return NodeRun{}, false, err
	}
	if w.state.Run.State != "running" {
		return NodeRun{}, false, errStopped
	}
	n, exists := w.state.Nodes[id]
	var facts []storage.Record
	if exists {
		if n.Kind != kind || n.Path != r.prefix+nodeID || n.InvocationID != r.scope.InvocationID {
			return n, false, product.NewError(product.CodeIncompatibleResume, "workflow node binding changed")
		}
		if n.State == "failed" {
			return n, false, product.NewError(n.ErrorCode, "workflow node already failed")
		}
		if n.State == "waiting" {
			n.State = "accepted"
			facts = append(facts, nodeFact(n))
		}
	}
	if !exists {
		n = NodeRun{ID: id, NodeID: nodeID, Path: r.prefix + nodeID, Kind: kind, DefinitionHash: r.scope.WorkflowDefinitionHash, State: "accepted", InvocationID: r.scope.InvocationID, Ordinal: 1}
		if kind == "tool" {
			n.ToolCallID = id
		}
		if kind == "subflow" {
			n.ChildInvocationID = agent.MustID()
		}
		facts = append(facts, nodeFact(n))
		if kind == "tool" {
			scope := r.scope
			scope.NodeExecutionID = id
			version := ""
			for _, def := range w.opts.Tools {
				if def.Name == name {
					version = def.Version
				}
			}
			call := agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: id, Name: name, Arguments: args, Generation: r.scope.Generation, Hash: rawHash([]byte(name + "\n" + version + "\n" + args + "\n" + r.scope.Generation))}}
			facts = append(facts, record("workflow_call", id, call))
		}
	}
	if kind == "subflow" && r.frame.subflows >= config.SubagentConcurrency {
		failure := product.NewError(product.CodeBudgetExhausted, "workflow subflow concurrency limit reached")
		n.State, n.ErrorCode = "failed", product.CodeBudgetExhausted
		facts = append(facts, nodeFact(n))
		if err := w.commitLocked(context.WithoutCancel(ctx), facts, []agent.Event{w.event("workflow.node.state_changed", id, map[string]any{"nodeExecutionId": id, "nodeId": nodeID, "kind": kind, "state": "failed", "errorCode": n.ErrorCode})}); err != nil {
			return n, false, err
		}
		return n, false, failure
	}
	if len(facts) > 0 {
		if err := w.commitLocked(context.WithoutCancel(ctx), facts, []agent.Event{w.event("workflow.node.state_changed", id, map[string]any{"nodeExecutionId": id, "nodeId": nodeID, "kind": kind, "state": "accepted"})}); err != nil {
			return n, false, err
		}
	}
	if kind == "subflow" {
		r.frame.subflows++
	}
	r.frame.nodes.Add(1)
	admitted = true
	return n, false, nil
}
func (r *workflowRun) done(kind string) {
	if kind != "subflow" {
		<-r.frame.semaphore
	} else {
		r.owner.mu.Lock()
		r.frame.subflows--
		r.owner.mu.Unlock()
	}
	r.frame.nodes.Done()
}
func (r *workflowRun) finish(ctx context.Context, id, result string, cause error) error {
	w := r.owner
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.state.Nodes[id]
	if w.active != r.frame || n.State != "accepted" {
		return product.NewError(product.CodeStateConflict, "workflow node is no longer executing")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	n.State = "completed"
	n.Result = result
	if cause != nil {
		n.State = "failed"
		n.ErrorCode = errorCode(cause)
	}
	return w.commitLocked(context.Background(), []storage.Record{nodeFact(n)}, []agent.Event{w.event("workflow.node.state_changed", id, map[string]any{"nodeExecutionId": id, "nodeId": n.NodeID, "kind": n.Kind, "state": n.State, "errorCode": n.ErrorCode})})
}
func (r *workflowRun) ledger(ctx context.Context, node NodeRun) *agent.BudgetLedger {
	r.owner.mu.Lock()
	limits := r.owner.state.Initial.Limits
	r.owner.mu.Unlock()
	budget := agent.NewBudget(limits)
	budget.Restore(node.Usage)
	last := node.Usage
	budget.SetPersist(func(next agent.Usage) error {
		w := r.owner
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.active != r.frame || r.frame.ctx.Err() != nil {
			return product.NewError(product.CodeStateConflict, "workflow budget segment is no longer active")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		aggregate := w.state.Usage
		aggregate.LogicalModelCalls += next.LogicalModelCalls - last.LogicalModelCalls
		aggregate.TransportRequests += next.TransportRequests - last.TransportRequests
		if aggregate.LogicalModelCalls > w.state.Initial.Limits.TraceLogicalModelCalls || aggregate.TransportRequests > w.state.Initial.Limits.TraceTransportRequests {
			return product.NewError(product.CodeBudgetExhausted, "workflow model budget exhausted")
		}
		p := budgetRecord{NodeID: node.ID, Local: next, Aggregate: aggregate}
		err := w.commitLocked(context.WithoutCancel(ctx), []storage.Record{record("workflow_budget", node.ID, p)}, nil)
		if err == nil {
			last = next
		}
		return err
	})
	return budget
}
func (r *workflowRun) RunModel(ctx context.Context, nodeID, binding, prompt string) (string, error) {
	n, saved, err := r.admit(ctx, nodeID, "model", "", "")
	if err != nil {
		return "", err
	}
	if saved {
		return n.Result, nil
	}
	defer r.done("model")
	// A complete assistant fact may have committed just before the segment
	// stopped. Recover that fact without starting another attempt or request.
	r.owner.mu.Lock()
	accepted, result := false, ""
	for _, attempt := range r.owner.state.Attempts {
		if attempt.Scope.NodeExecutionID == n.ID && attempt.Status == "complete" {
			accepted, result = true, modelText(attempt.Result.Message)
		}
	}
	r.owner.mu.Unlock()
	if accepted {
		if err := r.finish(ctx, n.ID, result, nil); err != nil {
			return "", err
		}
		return result, nil
	}
	scope := r.scope
	scope.NodeExecutionID = n.ID
	budget := r.ledger(ctx, n)
	validated, err := einorun.NewAuxiliaryModel(r.owner.opts.Models[binding], r.owner, budget, scope, "workflow_node", n.ID)
	if err != nil {
		return "", errors.Join(err, r.finish(ctx, n.ID, "", err))
	}
	requestCtx := einorun.WithExecutionScope(ctx, scope)
	input := []*schema.AgenticMessage{schema.UserAgenticMessage(prompt)}
	var msg *schema.AgenticMessage
	streaming := false
	if configured, ok := r.owner.opts.Models[binding].(interface{ Configuration() llm.ModelConfig }); ok {
		streaming = configured.Configuration().Capabilities.Items[llm.CapTextStream].Status == llm.Verified
	}
	if streaming {
		var reader *schema.StreamReader[*schema.AgenticMessage]
		reader, err = validated.Stream(requestCtx, input)
		if reader != nil {
			defer reader.Close()
		}
		if err == nil {
			msg, err = reader.Recv()
		}
	} else {
		msg, err = validated.Generate(requestCtx, input)
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return "", errors.Join(err, r.finish(ctx, n.ID, "", err))
	}
	text := modelText(msg)
	if err := r.finish(ctx, n.ID, text, nil); err != nil {
		return "", err
	}
	return text, nil
}
func modelText(msg *schema.AgenticMessage) string {
	var text strings.Builder
	for _, b := range msg.ContentBlocks {
		if b.AssistantGenText != nil {
			text.WriteString(b.AssistantGenText.Text)
		}
	}
	return text.String()
}
func (r *workflowRun) RunTool(ctx context.Context, nodeID, name string, args json.RawMessage) (string, error) {
	n, saved, err := r.admit(ctx, nodeID, "tool", name, string(args))
	if err != nil {
		return "", err
	}
	if saved {
		return n.Result, nil
	}
	defer r.done("tool")
	scope := r.scope
	scope.NodeExecutionID = n.ID
	budget := r.ledger(ctx, n)
	exec, err := tools.NewExecutor(scope.Generation, r.owner.opts.Tools, r.owner, workflowAuthorizer{owner: r.owner, scope: scope}, budget, tools.WithOperations(r.owner.opts.Operations), tools.WithResourceScheduler(r.owner.scheduler), tools.WithResourceDomain("trusted-injected", r.owner.opts.Workspace))
	if err != nil {
		return "", errors.Join(err, r.finish(ctx, n.ID, "", err))
	}
	out, err := exec.RunWorkflowNode(einorun.WithExecutionScope(ctx, scope), scope, n.ID, n.ToolCallID, name, string(args))
	var wait *agent.ApprovalWait
	if errors.As(err, &wait) {
		r.owner.mu.Lock()
		if r.owner.active == r.frame && r.owner.state.Run.State == "running" {
			run := r.owner.state.Run
			run.State = "pausing"
			err = r.owner.commitLocked(context.Background(), []storage.Record{record("workflow_run", r.owner.opts.RunID, run)}, nil)
		}
		r.owner.mu.Unlock()
		if err != nil && !errors.As(err, &wait) {
			return "", err
		}
		return "", errStopped
	}
	if ctx.Err() != nil {
		return "", errors.Join(err, ctx.Err())
	}
	if err == nil && out.Status != "succeeded" {
		code := product.CodeResourceUnavailable
		if out.Status == "denied" {
			code = product.CodePermissionDenied
		}
		if out.SideEffect == "unknown" || out.Status == "outcome_unknown" {
			code = product.CodeReconciliationRequired
		}
		err = product.NewError(code, "workflow tool node did not succeed")
	}
	if err != nil {
		return "", errors.Join(err, r.finish(ctx, n.ID, "", err))
	}
	r.owner.mu.Lock()
	result := r.owner.state.Calls[n.ToolCallID].ModelContent()
	r.owner.mu.Unlock()
	if err := r.finish(ctx, n.ID, result, nil); err != nil {
		return "", err
	}
	return result, nil
}
func (r *workflowRun) RunSubflow(ctx context.Context, nodeID string, c *CompiledWorkflow, input json.RawMessage) (map[string]any, error) {
	if r.depth >= config.SubagentDepth {
		return nil, product.NewError(product.CodeBudgetExhausted, "workflow subflow depth exceeded")
	}
	n, saved, err := r.admit(ctx, nodeID, "subflow", "", "")
	if err != nil {
		return nil, err
	}
	if saved {
		var out map[string]any
		d := json.NewDecoder(strings.NewReader(n.Result))
		d.UseNumber()
		err := d.Decode(&out)
		return out, err
	}
	defer r.done("subflow")
	if err := ValidateWorkflowInput(c, input); err != nil {
		return nil, errors.Join(err, r.finish(ctx, n.ID, "", err))
	}
	var in map[string]any
	d := json.NewDecoder(bytes.NewReader(input))
	d.UseNumber()
	if err := d.Decode(&in); err != nil {
		return nil, errors.Join(err, r.finish(ctx, n.ID, "", err))
	}
	child := *r
	child.prefix = r.prefix + nodeID + "/"
	child.depth++
	child.scope.WorkflowDefinitionHash = c.Hash
	child.scope.InvocationID = n.ChildInvocationID
	child.scope.ParentInvocationID = r.scope.InvocationID
	child.scope.NodeExecutionID = ""
	out, err := child.invoke(ctx, c, in)
	if err != nil {
		if errors.Is(err, errStopped) || ctx.Err() != nil {
			return nil, err
		}
		return nil, errors.Join(err, r.finish(ctx, n.ID, "", err))
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if err := r.finish(ctx, n.ID, string(raw), nil); err != nil {
		return nil, err
	}
	return out, nil
}
