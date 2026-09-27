package state

import (
	"context"
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// AcceptCommand commits the receipt and its queued input together. A missing
// revision remains zero in the request digest; only explicit revisions are CAS.
func (m *Manager) AcceptCommand(ctx context.Context, cmd OperationCommand, arguments json.RawMessage, target agent.TargetAgent, limits config.Limits) (OperationReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cmd.Kind != "direct_command" || cmd.Target == "" {
		return OperationReceipt{}, product.NewError(product.CodeInvalidArgument, "direct command identity is required")
	}
	if receipt, found, err := m.findOperation(cmd); found || err != nil {
		return receipt, err
	}
	if cmd.ExpectedRevision != 0 && cmd.ExpectedRevision != m.view.LastSeq {
		return OperationReceipt{}, product.NewError(product.CodeStateConflict, "session revision changed")
	}
	digest, err := operationDigest(cmd)
	if err != nil {
		return OperationReceipt{}, err
	}
	receipt := OperationReceipt{OperationID: agent.MustID(), State: "accepted", Target: cmd.Target, AcceptedCommit: m.view.LastSeq + 1}
	envelope, err := json.Marshal(struct {
		OperationID string          `json:"operationId"`
		Name        string          `json:"name"`
		Arguments   json.RawMessage `json:"arguments"`
	}{receipt.OperationID, cmd.Target, arguments})
	if err != nil {
		return OperationReceipt{}, product.NewError(product.CodeInvalidArgument, "invalid command arguments")
	}
	if _, _, err := m.classify(agent.InputCommand{Kind: "command", Content: envelope}, target); err != nil {
		return OperationReceipt{}, err
	}
	trace := TraceState{ID: agent.MustID(), State: "queued", Kind: "command", Target: target, Generation: target.Generation, Limits: limits.WithDefaults()}
	input := InputState{ID: agent.MustID(), TraceID: trace.ID, Kind: "command", State: "pending", Content: envelope, Principal: cmd.Principal, Target: target, CommitSeq: receipt.AcceptedCommit}
	inputReceipt := agent.InputReceipt{InputID: input.ID, TraceID: trace.ID, ActualKind: input.Kind, TargetAgent: target, State: input.State, AcceptedCommit: receipt.AcceptedCommit}
	op := Operation{Receipt: receipt, Principal: cmd.Principal, SessionID: m.sessionID, Kind: cmd.Kind, Key: cmd.IdempotencyKey, Digest: digest, Revision: 1, State: "accepted"}
	controls := []store.Record{record("operation", receipt.OperationID, op), record("input", input.ID, input), record("trace", trace.ID, trace)}
	op.Revision++
	op.State = "running"
	controls = append(controls, record("operation", receipt.OperationID, op))
	if _, err := m.commit(ctx, controls, nil, []agent.Event{m.event("input.accepted", trace.ID, "", inputReceipt)}); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

// SaveDirectCall records a trusted command invocation without fabricating a
// model turn or a FunctionToolCall message.
func (m *Manager) SaveDirectCall(ctx context.Context, call agent.ToolRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[call.Scope.TraceID]
	if tr == nil || tr.State != "running" || call.Call.CallID == "" || call.Call.OperationID == "" {
		return product.NewError(product.CodeStateConflict, "direct command is not running")
	}
	if _, exists := m.view.Calls[call.Call.CallID]; exists {
		return product.NewError(product.CodeStateConflict, "direct command call already exists")
	}
	_, err := m.commit(ctx, []store.Record{record("tool_call", call.Call.CallID, call)}, nil, nil)
	return err
}

// SaveCommand commits the command result, operation terminal and consumption
// together. Execution observations were already persisted by the Executor.
func (m *Manager) SaveCommand(ctx context.Context, scope agent.ExecutionScope, inputID, operationID, name string, content, result json.RawMessage, next, errorRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := m.view.Inputs[inputID]
	op, ok := m.view.Operations[operationID]
	call, hasCall := m.view.Calls[operationID]
	callMatches := call.Scope == scope
	if segment, resumed := m.view.ResumedExecutions[scope.ExecutionID]; resumed && segment.DirectResumeID != "" {
		b := m.view.DirectResumes[segment.DirectResumeID]
		callMatches = segment.Scope == scope && b.Scope == call.Scope && b.CallID == operationID
	}
	if in == nil || in.Kind != "command" || in.State != "pending" || in.TraceID != scope.TraceID || !ok || op.Kind != "direct_command" || op.Receipt.Target != name || !hasCall || !callMatches || call.Call.OperationID != operationID || !terminal(next) || !operationTransition(op.State, next) {
		return product.NewError(product.CodeStateConflict, "command result does not match its pending execution")
	}
	consumed := *in
	consumed.State = "consumed"
	msg := agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindCommand, Status: agent.StatusComplete,
		Scope:   agent.MessageScope{SessionID: m.sessionID, TraceID: scope.TraceID, InvocationID: scope.InvocationID, InputID: inputID, ToolCallID: operationID},
		Source:  agent.SourceRef{Kind: agent.SourceExtension, Description: "direct command"},
		Command: &agent.CommandMessage{Name: name, Content: append(json.RawMessage(nil), content...), Result: append(json.RawMessage(nil), result...)}}
	op.Revision++
	op.State, op.ResultRef, op.ErrorRef = next, msg.ID, errorRef
	entry := record("message", msg.ID, msg)
	entry.ParentID = m.view.LeafID
	_, err := m.commit(ctx, []store.Record{record("operation", operationID, op), record("input", inputID, consumed)}, []store.Record{entry}, []agent.Event{m.event("input.consumed", scope.TraceID, "", consumed), m.event("message.finalized", scope.TraceID, "", msg)})
	return err
}
