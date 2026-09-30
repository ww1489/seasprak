package state

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// WorkflowNodeRun is the durable identity and outcome of one executed
// workflow model or tool node. ID is the stable nodeExecutionId
// "<workflow invocation>:<node path>:<ordinal>"; retries and resume keep it.
// A completed record is the node's result: a re-run returns it instead of
// executing the node again. It is a control fact, never history.
type WorkflowNodeRun struct {
	ID           string `json:"id"`
	TraceID      string `json:"traceId"`
	InvocationID string `json:"invocationId"`
	NodeID       string `json:"nodeId"`
	Kind         string `json:"kind"`  // model | tool
	State        string `json:"state"` // accepted | waiting | completed | failed
	// A waiting tool node stopped its standalone workflow segment for a
	// runtime approval of ToolCallID. The call stays registered and unclaimed;
	// only an explicit Resume may run it, and only with a live approval.
	// ToolCallID is the product tool call of the current attempt of a tool
	// node. Attempt 1 uses ID itself; a retry after an attempt that provably
	// never started gets a new call, so each call's observation stays immutable.
	ToolCallID string `json:"toolCallId,omitempty"`
	Attempt    int    `json:"attempt,omitempty"`
	Result     string `json:"result,omitempty"`
	ModelCalls int    `json:"modelCalls,omitempty"`
	Error      string `json:"error,omitempty"`
	// ApprovalWait marks that the current attempt's call once waited for an
	// approval. Resume re-runs that same call (never a new attempt) and asks
	// again unless this process instance holds a live answer.
	ApprovalWait bool `json:"approvalWait,omitempty"`
}

func workflowNodeTerminal(state string) bool { return state == "completed" || state == "failed" }

// CommitWorkflowStop records that a standalone workflow segment stopped at a
// node boundary after its execution exited: the trace becomes paused with the
// stopped proof, an accepted pause operation completes, and an optional
// accepted tool node becomes waiting for its runtime approval, all in one
// commit. The waiting node's call must be registered, unclaimed, without an
// observation and frozen with a requested grant. Nothing grants permission.
func (m *Manager) CommitWorkflowStop(ctx context.Context, traceID, executionID, pauseID, waitingNodeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[traceID]
	if tr == nil || tr.State != "running" || !tr.Started || tr.Settled || tr.ExecutionStopped || executionID == "" || (tr.ExecutionID != "" && tr.ExecutionID != executionID) || m.view.TraceHasUnresolvedEffects(traceID) {
		return product.NewError(product.CodeStateConflict, "workflow trace is not a running execution")
	}
	var controls []store.Record
	if waitingNodeID != "" {
		node, ok := m.view.WorkflowNodes[waitingNodeID]
		call, registered := m.view.Calls[node.ToolCallID]
		frozen, saved := m.view.FrozenExecutions["execution:"+node.ToolCallID]
		if !ok || node.State != "accepted" || node.Kind != "tool" || node.TraceID != traceID || node.InvocationID != tr.InvocationID || !registered || call.Claimed || call.Observation != nil || !saved || frozen.RequestedGrantRef == "" || frozen.Scope != call.Scope || frozen.CallID != node.ToolCallID {
			return product.NewError(product.CodeStateConflict, "workflow node is not waiting for an original pending call")
		}
		node.State, node.ApprovalWait = "waiting", true
		controls = append(controls, record("workflow_node", node.ID, node))
	}
	next := *tr
	next.State, next.ExecutionID, next.ExecutionStopped, next.Settled, next.CheckpointID = "paused", executionID, true, false, ""
	controls = append(controls, record("trace", traceID, next))
	if pauseID != "" {
		op, exists := m.view.Operations[pauseID]
		if !exists || op.Kind != "pause" || op.Receipt.Target != traceID || op.State != "accepted" {
			return product.NewError(product.CodeStateConflict, "workflow pause operation is not accepted")
		}
		op.State, op.ResultRef = "completed", executionID
		op.Revision++
		controls = append(controls, record("operation", pauseID, op))
	}
	_, err := m.commit(ctx, controls, nil, []agent.Event{m.event("trace.state_changed", traceID, "", next)})
	return err
}

// ClaimWorkflowApprovedTool commits the claim of a workflow node call that an
// earlier stopped segment registered and a runtime approval now allows. The
// session mailbox owns and consumes the one-time permission after this
// commit; replay can never manufacture it. The node must be accepted again
// by Resume and the claiming execution must be the trace's current one.
func (m *Manager) ClaimWorkflowApprovedTool(ctx context.Context, frozen agent.FrozenCall, usage agent.Usage, executionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	call, exists := m.view.Calls[frozen.CallID]
	node, bound := m.view.WorkflowNodeForCall(frozen.CallID)
	tr := m.view.Traces[call.Scope.TraceID]
	if !exists || !bound || node.State != "accepted" || !node.ApprovalWait || node.ToolCallID != frozen.CallID || node.TraceID != call.Scope.TraceID || node.InvocationID != call.Scope.InvocationID || tr == nil || executionID == "" || tr.ExecutionID != executionID || tr.ExecutionStopped || call.Scope.ExecutionID == executionID || call.Scope.TurnID != "" || call.Call.ProviderCallID != "" {
		return product.NewError(product.CodePermissionDenied, "runtime claim is not an original workflow node call")
	}
	return m.claimTool(ctx, frozen, usage, true)
}

// WorkflowToolRetryable reports whether a workflow tool call ended without
// starting and without effects, so the node may use a new attempt.
func WorkflowToolRetryable(call agent.ToolRecord) bool {
	o := call.Observation
	return !call.Claimed && o != nil && !o.Executed && o.SideEffect == "none" && (o.Status == "cancelled" || o.Status == "skipped")
}

// workflowNodeScopeValid requires a running trace whose root invocation, or a
// running delegated invocation of that trace, owns the node.
func (m *Manager) workflowNodeScopeValid(node WorkflowNodeRun) bool {
	tr := m.view.Traces[node.TraceID]
	if tr == nil || tr.State != "running" {
		return false
	}
	if node.InvocationID == tr.InvocationID {
		return true
	}
	inv, ok := m.view.Invocations[node.InvocationID]
	return ok && inv.State == "running" && inv.TraceID == node.TraceID
}

// BeginWorkflowNode commits an accepted node before it runs. For a tool node
// the product tool call is registered in the same commit, so the controlled
// executor finds an accepted call whose binding comes from the definition,
// not from a model response. Re-beginning an accepted tool node is allowed
// only for the next attempt after a call that provably never started.
func (m *Manager) BeginWorkflowNode(ctx context.Context, node WorkflowNodeRun, call *agent.ToolRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if node.ID == "" || node.NodeID == "" || node.TraceID == "" || node.InvocationID == "" || node.State != "accepted" || node.Result != "" || node.Error != "" || node.ModelCalls != 0 {
		return product.NewError(product.CodeInvalidArgument, "workflow node identity is incomplete")
	}
	switch node.Kind {
	case "model":
		if call != nil || node.ToolCallID != "" || node.Attempt != 0 {
			return product.NewError(product.CodeInvalidArgument, "only tool nodes register tool calls")
		}
	case "tool":
		if call == nil || node.ToolCallID == "" || node.Attempt < 1 {
			return product.NewError(product.CodeInvalidArgument, "tool node requires its tool call")
		}
	default:
		return product.NewError(product.CodeInvalidArgument, "workflow node kind is unsupported")
	}
	if old, ok := m.view.WorkflowNodes[node.ID]; ok {
		previous, found := m.view.Calls[old.ToolCallID]
		if old.State != "accepted" || old.Kind != "tool" || old.TraceID != node.TraceID || old.InvocationID != node.InvocationID || old.NodeID != node.NodeID || node.Attempt != old.Attempt+1 || !found || !WorkflowToolRetryable(previous) {
			return product.NewError(product.CodeStateConflict, "workflow node transition is invalid")
		}
	} else if node.Attempt > 1 {
		return product.NewError(product.CodeStateConflict, "workflow node transition is invalid")
	}
	if !m.workflowNodeScopeValid(node) {
		return product.NewError(product.CodeStateConflict, "workflow node requires a running invocation")
	}
	controls := []store.Record{record("workflow_node", node.ID, node)}
	var events []agent.Event
	if call != nil {
		c := call.Call
		if _, exists := m.view.Calls[c.CallID]; exists || c.CallID != node.ToolCallID || c.ProviderCallID != "" || c.OperationID != "" || c.Name == "" || c.SelectionRevision != 0 || call.Claimed || call.Observation != nil ||
			call.Scope.TraceID != node.TraceID || call.Scope.InvocationID != node.InvocationID || call.Scope.TurnID != "" || call.Scope.SelectionRevision != 0 {
			return product.NewError(product.CodeStateConflict, "workflow tool call cannot be registered")
		}
		controls = append(controls, record("tool_call", c.CallID, *call))
		events = append(events, m.event("tool.requested", node.TraceID, "", *call))
	}
	_, err := m.commit(ctx, controls, nil, events)
	return err
}

// FinishWorkflowNode commits the single terminal transition of an accepted node.
func (m *Manager) FinishWorkflowNode(ctx context.Context, node WorkflowNodeRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.view.WorkflowNodes[node.ID]
	if !ok || old.State != "accepted" || !workflowNodeTerminal(node.State) || old.TraceID != node.TraceID || old.InvocationID != node.InvocationID || old.NodeID != node.NodeID || old.Kind != node.Kind || old.ToolCallID != node.ToolCallID || old.Attempt != node.Attempt || node.ModelCalls < 0 {
		return product.NewError(product.CodeStateConflict, "workflow node transition is invalid")
	}
	if !m.workflowNodeScopeValid(node) {
		return product.NewError(product.CodeStateConflict, "workflow node requires a running invocation")
	}
	_, err := m.commit(ctx, []store.Record{record("workflow_node", node.ID, node)}, nil, nil)
	return err
}

func applyWorkflowNode(v *View, r store.Record) error {
	var node WorkflowNodeRun
	if err := json.Unmarshal(r.Payload, &node); err != nil {
		return err
	}
	if node.ID == "" || node.ID != r.ID {
		return product.NewError(product.CodeIncompatibleVersion, "workflow node identity mismatch")
	}
	if old, ok := v.WorkflowNodes[node.ID]; ok && (workflowNodeTerminal(old.State) || old.NodeID != node.NodeID || old.TraceID != node.TraceID || old.InvocationID != node.InvocationID || old.Kind != node.Kind) {
		return product.NewError(product.CodeStateConflict, "workflow node record is immutable")
	}
	if v.WorkflowNodes == nil {
		v.WorkflowNodes = map[string]WorkflowNodeRun{}
	}
	v.WorkflowNodes[node.ID] = node
	return nil
}

// WorkflowNodeForCall returns the workflow tool node that registered callID
// in any attempt (attempt 1 is the node ID, attempt k is "<ID>#k"). Model and
// direct tool calls never have one.
func (v View) WorkflowNodeForCall(callID string) (WorkflowNodeRun, bool) {
	if callID == "" {
		return WorkflowNodeRun{}, false
	}
	if node, ok := v.WorkflowNodes[callID]; ok && node.Kind == "tool" {
		return node, true
	}
	// Node IDs are free text, so only a trailing all-digit "#k" is an attempt.
	cut := strings.LastIndexByte(callID, '#')
	if cut <= 0 || cut == len(callID)-1 || strings.Trim(callID[cut+1:], "0123456789") != "" {
		return WorkflowNodeRun{}, false
	}
	node, ok := v.WorkflowNodes[callID[:cut]]
	if !ok || node.Kind != "tool" {
		return WorkflowNodeRun{}, false
	}
	return node, true
}

// CommitWorkflowResume accepts Resume of a stopped paused workflow trace and
// starts its new execution segment in one commit. Workflows have no runner
// checkpoint: the saved node records are their progress, and completed nodes
// are reused rather than executed again.
func (m *Manager) CommitWorkflowResume(ctx context.Context, cmd OperationCommand, executionID string) (OperationReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if receipt, found, err := m.findOperation(cmd); found || err != nil {
		return receipt, err
	}
	if cmd.Kind != "resume" || cmd.ExpectedRevision != m.view.LastSeq {
		return OperationReceipt{}, product.NewError(product.CodeStateConflict, "invalid resume operation or revision")
	}
	tr := m.view.Traces[cmd.Target]
	if tr == nil || tr.State != "paused" || !tr.Started || tr.Settled || !tr.ExecutionStopped || executionID == "" || executionID == tr.ExecutionID || m.view.ActiveTrace != tr.ID || m.view.HasUnresolvedEffects() {
		return OperationReceipt{}, product.NewError(product.CodeIncompatibleResume, "workflow trace is not a stopped paused execution")
	}
	receipt := OperationReceipt{OperationID: agent.MustID(), State: "accepted", Target: tr.ID, AcceptedCommit: m.view.LastSeq + 1}
	digest, err := operationDigest(cmd)
	if err != nil {
		return OperationReceipt{}, err
	}
	op := Operation{Receipt: receipt, Principal: cmd.Principal, SessionID: m.sessionID, Kind: "resume", Key: cmd.IdempotencyKey, Digest: digest, Revision: 1, State: "accepted"}
	next := *tr
	next.State, next.ExecutionID, next.CheckpointID, next.ExecutionStopped = "running", executionID, "", false
	controls := []store.Record{record("operation", receipt.OperationID, op), record("trace", tr.ID, next)}
	// A waiting node runs again in the new segment: it becomes accepted in the
	// same commit, keeping its call and approval-wait marker.
	for id, node := range m.view.WorkflowNodes {
		if node.TraceID == tr.ID && node.State == "waiting" {
			node.State = "accepted"
			controls = append(controls, record("workflow_node", id, node))
		}
	}
	_, err = m.commit(ctx, controls, nil, []agent.Event{m.event("trace.state_changed", tr.ID, "", next)})
	if err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
