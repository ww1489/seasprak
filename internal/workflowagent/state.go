package workflowagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/storage"
)

type initialRecord struct {
	RunID          string               `json:"runId"`
	Workspace      string               `json:"workspace"`
	Manifest       manifest             `json:"manifest"`
	BindingVersion string               `json:"bindingVersion"`
	Principal      string               `json:"principal"`
	Policy         agent.ResolvedPolicy `json:"policy"`
	Limits         config.Limits        `json:"limits"`
}
type inputRecord struct {
	Receipt   WorkflowInputReceipt `json:"receipt"`
	Input     json.RawMessage      `json:"input"`
	Principal string               `json:"principal"`
	Key       string               `json:"key"`
	Digest    string               `json:"digest"`
}
type runRecord struct {
	ActivityUsed     time.Duration   `json:"activityUsed"`
	State            string          `json:"state"`
	ExecutionID      string          `json:"executionId,omitempty"`
	InvocationID     string          `json:"invocationId,omitempty"`
	ExecutionStopped bool            `json:"executionStopped"`
	Result           json.RawMessage `json:"result,omitempty"`
	ErrorCode        string          `json:"errorCode,omitempty"`
	FailedNode       string          `json:"failedNode,omitempty"`
}
type nodeRecord struct {
	Node   NodeRun     `json:"node"`
	Result string      `json:"result,omitempty"`
	Usage  agent.Usage `json:"usage"`
}
type budgetRecord struct {
	NodeID    string      `json:"nodeExecutionId"`
	Local     agent.Usage `json:"local"`
	Aggregate agent.Usage `json:"aggregate"`
}
type attemptRecord struct {
	Scope    agent.ExecutionScope       `json:"scope"`
	Identity agent.ModelAttemptIdentity `json:"identity"`
	Status   string                     `json:"status"`
	Result   *modelResult               `json:"result,omitempty"`
}
type operationRecord struct {
	Receipt   WorkflowOperationReceipt `json:"receipt"`
	Kind      string                   `json:"kind"`
	State     string                   `json:"state"`
	Principal string                   `json:"principal"`
	Key       string                   `json:"key"`
	Digest    string                   `json:"digest"`
}
type toolIntent struct {
	Scope  agent.ExecutionScope `json:"scope"`
	Call   agent.FrozenCall     `json:"call"`
	Budget budgetRecord         `json:"budget"`
}
type runState struct {
	Initial        initialRecord
	Initialized    bool
	Run            runRecord
	Input          *inputRecord
	Nodes          map[string]NodeRun
	Calls          map[string]agent.ToolRecord
	Frozen         map[string]agent.FrozenExecution
	Attempts       map[string]attemptRecord
	Requests       map[string][]llm.TransportRequest
	Operations     map[string]operationRecord
	Todos          map[string]todoUpdate
	TodoUpdates    map[string]todoUpdate
	Usage          agent.Usage
	Revision       uint64
	Cursor         uint64
	Events         []agent.Event
	RepairRequired bool
}

func newRunState() runState {
	return runState{Nodes: map[string]NodeRun{}, Calls: map[string]agent.ToolRecord{}, Frozen: map[string]agent.FrozenExecution{}, Attempts: map[string]attemptRecord{}, Requests: map[string][]llm.TransportRequest{}, Operations: map[string]operationRecord{}, Todos: map[string]todoUpdate{}, TodoUpdates: map[string]todoUpdate{}}
}
func copyMap[K comparable, V any](src map[K]V) map[K]V {
	out := make(map[K]V, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
func (s runState) clone() runState {
	s.Nodes = copyMap(s.Nodes)
	s.Calls = copyMap(s.Calls)
	s.Frozen = copyMap(s.Frozen)
	s.Attempts = copyMap(s.Attempts)
	s.Requests = copyMap(s.Requests)
	for id, requests := range s.Requests {
		s.Requests[id] = append([]llm.TransportRequest(nil), requests...)
	}
	s.Operations = copyMap(s.Operations)
	s.Todos = copyMap(s.Todos)
	for id, u := range s.Todos {
		u.Content = append(json.RawMessage(nil), u.Content...)
		s.Todos[id] = u
	}
	s.TodoUpdates = copyMap(s.TodoUpdates)
	for id, u := range s.TodoUpdates {
		u.Content = append(json.RawMessage(nil), u.Content...)
		s.TodoUpdates[id] = u
	}
	s.Events = append([]agent.Event(nil), s.Events...)
	return s
}
func record(kind, id string, v any) storage.Record {
	raw, _ := json.Marshal(v)
	return storage.Record{Type: kind, Version: 1, ID: id, Payload: raw}
}
func nodeFact(n NodeRun) storage.Record {
	return record("workflow_node", n.ID, nodeRecord{Node: n, Result: n.Result, Usage: n.Usage})
}
func invalidRecord() error {
	return product.NewError(product.CodeIncompatibleVersion, "invalid workflow record or state transition")
}
func decode(raw json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return invalidRecord()
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return invalidRecord()
	}
	return nil
}
func scopeRoot(s agent.ExecutionScope, id string) bool {
	return s.WorkflowRunID == id && s.SessionID == "" && s.BranchID == "" && s.TraceID == "" && s.TurnID == "" && s.SelectionRevision == 0 && s.InvocationID != "" && s.ExecutionID != "" && s.Generation != ""
}
func (s *runState) validNodeScope(scope agent.ExecutionScope) bool {
	n, ok := s.Nodes[scope.NodeExecutionID]
	return scopeRoot(scope, s.Initial.RunID) && ok && n.InvocationID == scope.InvocationID && n.DefinitionHash == scope.WorkflowDefinitionHash && scope.Generation == s.Initial.BindingVersion
}
func (s *runState) apply(r storage.Record) error {
	if r.Version != 1 || r.ID == "" || r.Type == "" {
		return invalidRecord()
	}
	if !s.Initialized && r.Type != "workflow_initialized" {
		return invalidRecord()
	}
	switch r.Type {
	case "workflow_initialized":
		var p initialRecord
		if decode(r.Payload, &p) != nil || s.Initialized || r.ID != "initial" || p.RunID == "" || p.Workspace == "" || p.Manifest.ResourceType != storage.ResourceWorkflow || p.Manifest.FormatVersion != 1 || p.Manifest.TodoBackend != workflowTodoBackend && p.Manifest.TodoBackend != "host-injected" || p.Manifest.DefinitionHash == "" || p.BindingVersion != digest(p.Manifest) || p.Policy.Ref == "" || p.Policy.Revision != 1 {
			return invalidRecord()
		}
		policy, err := normalizePolicy(&p.Policy)
		if err != nil || policy != p.Policy {
			return invalidRecord()
		}
		s.Initial = p
		s.Initialized = true
		s.Run = runRecord{State: "created", ExecutionStopped: true}
	case "workflow_input":
		var p inputRecord
		if decode(r.Payload, &p) != nil || s.Input != nil || s.Run.State != "created" || r.ID != p.Receipt.InputID || p.Receipt.RunID != s.Initial.RunID || p.Receipt.DefinitionVersion != s.Initial.Manifest.Definition.Version || p.Receipt.BindingVersion != s.Initial.BindingVersion || p.Receipt.State != "accepted" || p.Receipt.AcceptedCommit != s.Revision+1 || p.Principal != s.Initial.Principal || !json.Valid(p.Input) || p.Digest != rawHash(p.Input) {
			return invalidRecord()
		}
		s.Input = &p
	case "workflow_run":
		var p runRecord
		if decode(r.Payload, &p) != nil || r.ID != s.Initial.RunID || !validRunTransition(s.Run, p, s.Input != nil) {
			return invalidRecord()
		}
		if p.State == "completed" && (len(p.Result) == 0 || !json.Valid(p.Result) || s.hasUnknown()) {
			return invalidRecord()
		}
		if p.State == "failed" && (p.ErrorCode == "" || !publicCode(p.ErrorCode)) {
			return invalidRecord()
		}
		if terminal(p.State) && !p.ExecutionStopped {
			return invalidRecord()
		}
		s.Run = p
	case "workflow_node":
		var p nodeRecord
		if decode(r.Payload, &p) != nil {
			return invalidRecord()
		}
		n := p.Node
		n.Result = p.Result
		n.Usage = p.Usage
		if n.ID != r.ID || n.ID == "" || n.Path == "" || n.NodeID == "" || n.InvocationID == "" || n.Ordinal != 1 || s.Run.ExecutionStopped || terminal(s.Run.State) {
			return invalidRecord()
		}
		previous, exists := s.Nodes[n.ID]
		if !exists {
			if n.State != "accepted" || n.Usage != (agent.Usage{}) || (n.Kind != "model" && n.Kind != "tool" && n.Kind != "subflow") || n.Kind == "tool" && n.ToolCallID != n.ID || n.Kind != "tool" && n.ToolCallID != "" {
				return invalidRecord()
			}
		} else {
			before := previous
			before.State = n.State
			before.Result = n.Result
			before.ErrorCode = n.ErrorCode
			if !reflect.DeepEqual(before, n) || !validNodeTransition(previous.State, n.State) {
				return invalidRecord()
			}
		}
		if n.State == "waiting" {
			f, frozen := s.Frozen["execution:"+n.ToolCallID]
			call := s.Calls[n.ToolCallID]
			if n.Kind != "tool" || !frozen || f.RequestedGrantRef == "" || call.Claimed || call.Observation != nil {
				return invalidRecord()
			}
		}
		if n.State == "completed" && n.Kind == "tool" {
			call := s.Calls[n.ToolCallID]
			if call.Observation == nil || call.Observation.Status != "succeeded" || call.Observation.SideEffect == "unknown" || n.Result != call.ModelContent() {
				return invalidRecord()
			}
		}
		if n.State == "completed" && n.Kind == "model" {
			accepted := 0
			for _, attempt := range s.Attempts {
				if attempt.Scope.NodeExecutionID == n.ID && attempt.Status == "complete" {
					if attempt.Result == nil || n.Result != modelText(attempt.Result.Message) {
						return invalidRecord()
					}
					accepted++
				}
			}
			if accepted != 1 {
				return invalidRecord()
			}
		}
		if n.State == "completed" && n.Kind == "subflow" {
			definition, ok := s.definitionForNode(n)
			if !ok {
				return invalidRecord()
			}
			var child WorkflowDefinition
			for _, declared := range definition.Nodes {
				if declared.ID == n.NodeID {
					child = s.Initial.Manifest.Subflows[declared.Subflow]
				}
			}
			actual, err := canonicalInput(json.RawMessage(n.Result))
			if err != nil {
				return invalidRecord()
			}
			for _, end := range child.Nodes {
				if end.Type == "end" {
					result, err := s.projectValues(n.ChildInvocationID, child, end.Inputs)
					if err != nil {
						return invalidRecord()
					}
					result, err = canonicalInput(result)
					if err != nil || !bytes.Equal(actual, result) {
						return invalidRecord()
					}
				}
			}
		}
		s.Nodes[n.ID] = n
	case "workflow_call":
		var p agent.ToolRecord
		if decode(r.Payload, &p) != nil || r.ID != p.Call.CallID || p.Claimed || p.Observation != nil || !s.validNodeScope(p.Scope) || p.Scope.NodeExecutionID != p.Call.CallID || p.Call.ProviderCallID != "" || p.Call.OperationID != "" || p.Call.Generation != s.Initial.BindingVersion || p.Call.Name == "" || !json.Valid([]byte(p.Call.Arguments)) {
			return invalidRecord()
		}
		if _, exists := s.Calls[r.ID]; exists {
			return invalidRecord()
		}
		n := s.Nodes[p.Scope.NodeExecutionID]
		if n.Kind != "tool" || n.ToolCallID != p.Call.CallID || n.State != "accepted" {
			return invalidRecord()
		}
		s.Calls[r.ID] = p
	case "workflow_frozen":
		var p agent.FrozenExecution
		if decode(r.Payload, &p) != nil {
			return invalidRecord()
		}
		call, ok := s.Calls[p.CallID]
		if !ok || call.Claimed || call.Observation != nil || p.ID != r.ID || p.ID != "execution:"+p.CallID || p.Scope != call.Scope || p.Origin != "workflow_node" || p.NodeExecutionID != call.Scope.NodeExecutionID || p.DefinitionRef != call.Scope.WorkflowDefinitionHash || p.BindingRef != s.Initial.BindingVersion || p.ProviderCallID != "" || p.OperationID != "" || p.Tool != call.Call.Name || p.Generation != s.Initial.BindingVersion || p.PolicyRef != s.Initial.Policy.Ref || p.OriginalArgumentsHash != rawHash([]byte(call.Call.Arguments)) || p.FinalArgumentsHash != rawHash(p.FinalArguments) {
			return invalidRecord()
		}
		hash, e := p.Digest()
		if e != nil || p.Hash == "" || hash != p.Hash {
			return invalidRecord()
		}
		if old, ok := s.Frozen[p.ID]; ok && old.Hash != p.Hash {
			return invalidRecord()
		}
		s.Frozen[p.ID] = p.Clone()
	case "workflow_budget":
		var p budgetRecord
		if decode(r.Payload, &p) != nil || r.ID != p.NodeID || s.applyBudget(p, false) != nil {
			return invalidRecord()
		}
	case "workflow_tool_intent":
		var p toolIntent
		if decode(r.Payload, &p) != nil {
			return invalidRecord()
		}
		call, ok := s.Calls[r.ID]
		f, found := s.Frozen["execution:"+r.ID]
		if !ok || !found || call.Claimed || call.Observation != nil || p.Call != call.Call || p.Scope != call.Scope || !s.validNodeScope(p.Scope) || p.Budget.NodeID != p.Scope.NodeExecutionID || f.Hash == "" || s.applyBudget(p.Budget, true) != nil {
			return invalidRecord()
		}
		call.Claimed = true
		s.Calls[r.ID] = call
	case "workflow_todo":
		var p todoUpdate
		if decode(r.Payload, &p) != nil || r.ID != p.CallID || s.validateTodo(p) != nil {
			return invalidRecord()
		}
		p.Content = append(json.RawMessage(nil), p.Content...)
		s.Todos[p.InvocationID] = p
		s.TodoUpdates[p.CallID] = p
	case "workflow_tool_observation":
		var p agent.ToolRecord
		if decode(r.Payload, &p) != nil {
			return invalidRecord()
		}
		call, ok := s.Calls[r.ID]
		if !ok || call.Observation != nil || p.Call != call.Call || p.Scope != call.Scope || p.Claimed != call.Claimed || p.Observation == nil || !validObservation(*p.Observation) {
			return invalidRecord()
		}
		if !p.Claimed && (p.Observation.Executed || p.Observation.SideEffect != "none") {
			return invalidRecord()
		}
		if s.Initial.Manifest.TodoBackend == workflowTodoBackend && call.Call.Name == "write_todos" && s.Frozen["execution:"+r.ID].BackendID == "todo-operations" {
			u, updated := s.TodoUpdates[r.ID]
			o := p.Observation
			if updated && o.SideEffect == "none" || o.SideEffect == "confirmed" && (!updated || !o.Executed) || o.Status == "succeeded" && (!updated || o.SideEffect != "confirmed" || !o.Executed || o.Content != string(u.Content)) {
				return invalidRecord()
			}
		}
		s.Calls[r.ID] = p
	case "workflow_tool_projection":
		var p agent.ToolOutputProjection
		if decode(r.Payload, &p) != nil || p.CallID != r.ID {
			return invalidRecord()
		}
		call, ok := s.Calls[r.ID]
		if !ok || call.Observation == nil || call.Projection != nil || p.Observation != *call.Observation || p.Artifact.ID != "" && (p.Artifact.SessionID != "" || p.Artifact.WorkflowRunID != s.Initial.RunID) {
			return invalidRecord()
		}
		call.Projection = &p
		s.Calls[r.ID] = call
	case "workflow_model_attempt":
		var p attemptRecord
		if decode(r.Payload, &p) != nil || p.Identity.ID != r.ID || p.Identity.ModelCallID != p.Scope.NodeExecutionID || p.Identity.Purpose != "workflow_node" || p.Identity.MessageID == "" || p.Identity.StreamID == "" || !s.validNodeScope(p.Scope) || s.Nodes[p.Scope.NodeExecutionID].Kind != "model" || s.Nodes[p.Scope.NodeExecutionID].State != "accepted" {
			return invalidRecord()
		}
		prior, ok := s.Attempts[r.ID]
		if !ok {
			if p.Status != "started" || p.Result != nil || s.Nodes[p.Scope.NodeExecutionID].Usage.ModelCallID != p.Identity.ModelCallID {
				return invalidRecord()
			}
			for _, prior := range s.Attempts {
				if prior.Scope.NodeExecutionID == p.Scope.NodeExecutionID && (prior.Status == "started" || prior.Status == "complete") {
					return invalidRecord()
				}
			}
		} else {
			if prior.Status != "started" || prior.Identity != p.Identity || prior.Scope != p.Scope || p.Result == nil || p.Result.AttemptID != r.ID || p.Result.Status != p.Status || !validAttemptStatus(p.Status) {
				return invalidRecord()
			}
			if p.Status == "complete" && (agent.ValidateModelResponse(p.Result.Message, false) != nil || p.Result.Details.FailureCode != "" || len(s.Requests[r.ID]) == 0) {
				return invalidRecord()
			}
			seen := map[uint64]bool{}
			for _, usage := range p.Result.Details.Usage {
				request := usage.Request
				requests := s.Requests[r.ID]
				if request.TransportAttempt == 0 || request.TransportAttempt > uint64(len(requests)) || requests[request.TransportAttempt-1] != request || seen[request.TransportAttempt] {
					return invalidRecord()
				}
				seen[request.TransportAttempt] = true
				if _, err := (llm.UsageRecord{}).MergeCumulative(usage.Snapshot.Usage); err != nil {
					return invalidRecord()
				}
			}
		}
		s.Attempts[r.ID] = p
	case "workflow_operation":
		var p operationRecord
		if decode(r.Payload, &p) != nil || p.Receipt.OperationID != r.ID || p.Receipt.Target != s.Initial.RunID || p.Receipt.ReceiptScope != "durable" || p.Receipt.InstanceID != "" || p.Principal != s.Initial.Principal || (p.Kind != "pause" && p.Kind != "cancel" && p.Kind != "resume") {
			return invalidRecord()
		}
		old, ok := s.Operations[r.ID]
		if !ok {
			if p.State != "running" || p.Receipt.AcceptedCommit != s.Revision+1 {
				return invalidRecord()
			}
		} else {
			copy := old
			copy.State = p.State
			if copy != p || old.State != "running" || (p.State != "completed" && p.State != "failed") {
				return invalidRecord()
			}
		}
		s.Operations[r.ID] = p
	default:
		return invalidRecord()
	}
	return nil
}
func validRunTransition(old, next runRecord, hasInput bool) bool {
	if next.ActivityUsed < old.ActivityUsed || next.ActivityUsed < 0 || (!next.ExecutionStopped || old.ExecutionStopped) && next.ActivityUsed != old.ActivityUsed {
		return false
	}
	if next.State == "running" {
		if !hasInput || next.ExecutionID == "" || next.InvocationID == "" || next.ExecutionStopped {
			return false
		}
		if old.State == "created" {
			return true
		}
		return old.State == "paused" && old.ExecutionStopped && next.InvocationID == old.InvocationID && next.ExecutionID != old.ExecutionID
	}
	if next.ExecutionID != old.ExecutionID || next.InvocationID != old.InvocationID {
		return false
	}
	switch old.State {
	case "running":
		return next.State == "pausing" || next.State == "cancelling" || next.State == "paused" || next.State == "completed" || next.State == "failed"
	case "pausing":
		return next.State == "paused" || next.State == "cancelling" || next.State == "failed"
	case "cancelling":
		return next.State == "cancelled"
	case "paused":
		return next.State == "cancelling" || next.State == "cancelled"
	case "created":
		return next.State == "cancelled"
	}
	return false
}
func validNodeTransition(old, next string) bool {
	switch old {
	case "accepted":
		return next == "completed" || next == "failed" || next == "waiting"
	case "waiting":
		return next == "accepted"
	}
	return false
}
func validAttemptStatus(s string) bool {
	return s == "complete" || s == "failed" || s == "incomplete" || s == "aborted"
}
func validObservation(o agent.ToolObservation) bool {
	switch o.Status {
	case "succeeded", "failed", "denied", "cancelled", "timed_out", "outcome_unknown", "skipped":
	default:
		return false
	}
	return o.SideEffect == "none" || o.SideEffect == "confirmed" || o.SideEffect == "unknown"
}
func terminal(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled"
}
func publicCode(c string) bool {
	switch c {
	case product.CodeInvalidArgument, product.CodeUnauthenticated, product.CodePermissionDenied, product.CodeNotFound, product.CodeStateConflict, product.CodeIdempotencyConflict, product.CodeUnsupportedCapability, product.CodeBudgetExhausted, product.CodeStorageUnavailable, product.CodeIncompatibleVersion, product.CodeIncompatibleResume, product.CodeReconciliationRequired, product.CodeResyncRequired, product.CodeResourceUnavailable, product.CodeInternal:
		return true
	}
	return false
}
func errorCode(err error) string {
	var e *product.Error
	if errors.As(err, &e) && publicCode(e.Code) {
		return e.Code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return product.CodeStateConflict
	}
	return product.CodeInternal
}
func (s runState) hasUnknown() bool {
	for _, c := range s.Calls {
		if c.Claimed && (c.Observation == nil || c.Observation.SideEffect == "unknown" || c.Observation.Status == "outcome_unknown" || c.Observation.Process && !c.Observation.Terminated && (c.Observation.Executed || c.Observation.SideEffect != "none")) {
			return true
		}
	}
	return false
}
func (s *runState) applyBudget(p budgetRecord, tool bool) error {
	n, ok := s.Nodes[p.NodeID]
	if !ok || n.State != "accepted" || tool && n.Kind != "tool" || !tool && n.Kind != "model" {
		return invalidRecord()
	}
	before := n.Usage
	l, t, x := p.Local.LogicalModelCalls-before.LogicalModelCalls, p.Local.TransportRequests-before.TransportRequests, p.Local.ToolExecutions-before.ToolExecutions
	if l < 0 || t < 0 || x < 0 || l > 1 || t > 1 || x > 1 || tool && (x != 1 || l != 0 || t != 0) || !tool && (x != 0 || l+t != 1) {
		return invalidRecord()
	}
	if tool {
		if p.Local.ModelCallID != before.ModelCallID || p.Local.ModelRequests != before.ModelRequests || p.Local.LastTransport != before.LastTransport {
			return invalidRecord()
		}
	} else {
		if p.Local.ModelCallID != n.ID || p.Local.ModelRequests != before.ModelRequests+t || p.Local.LogicalModelCalls != 1 || l == 1 && (before.ModelCallID != "" || p.Local.LastTransport != (llm.TransportRequest{})) || t == 1 && before.ModelCallID != n.ID {
			return invalidRecord()
		}
		if t == 1 {
			var current attemptRecord
			for _, attempt := range s.Attempts {
				if attempt.Scope.NodeExecutionID == n.ID && attempt.Status == "started" {
					if current.Identity.ID != "" {
						return invalidRecord()
					}
					current = attempt
				}
			}
			if current.Identity.ID == "" {
				return invalidRecord()
			}
			request := p.Local.LastTransport
			requests := s.Requests[current.Identity.ID]
			binding, ok := s.modelForNode(n)
			if !ok {
				return invalidRecord()
			}
			if binding.Observed {
				if request.ModelCallID != n.ID || request.AttemptID != current.Identity.ID || (request.Purpose != current.Identity.Purpose && request.Purpose != "cache_create") || request.TransportAttempt != uint64(len(requests)+1) {
					return invalidRecord()
				}
			} else if request != (llm.TransportRequest{}) || len(requests) != 0 {
				return invalidRecord()
			}
			s.Requests[current.Identity.ID] = append(requests, request)
		}
	}
	expected := s.Usage
	expected.LogicalModelCalls += l
	expected.TransportRequests += t
	expected.ToolExecutions += x
	limits := s.Initial.Limits
	if expected != p.Aggregate || expected.LogicalModelCalls > limits.TraceLogicalModelCalls || expected.TransportRequests > limits.TraceTransportRequests || expected.ToolExecutions > limits.TraceToolCalls || p.Local.ModelRequests > limits.LogicalModelRequests {
		return invalidRecord()
	}
	n.Usage = p.Local
	s.Nodes[n.ID] = n
	s.Usage = expected
	return nil
}
func replay(loaded storage.StoredSession, id string) (runState, error) {
	s := newRunState()
	for _, c := range loaded.Commits {
		if c.CommitSeq != s.Revision+1 || c.ExpectedPreviousSeq != s.Revision || len(c.Entries) != 0 || len(c.BranchUpdates) != 0 {
			return s, invalidRecord()
		}
		for _, r := range c.ControlRecords {
			if err := s.apply(r); err != nil {
				return s, err
			}
		}
		if err := s.validateCombination(); err != nil {
			return s, err
		}
		for _, ev := range c.Events {
			if err := ev.Validate(); err != nil || ev.Scope.WorkflowRunID != id || ev.DurableSeq == nil || *ev.DurableSeq != s.Cursor+1 || ev.StreamID != "" || ev.ChunkSeq != nil {
				return s, invalidRecord()
			}
			s.Cursor = *ev.DurableSeq
			s.Events = append(s.Events, cloneEvent(ev))
		}
		s.Revision = c.CommitSeq
	}
	if !s.Initialized || s.Initial.RunID != id {
		return s, invalidRecord()
	}
	s.RepairRequired = loaded.RepairRequired
	return s, nil
}
func (w *WorkflowAgent) commitLocked(ctx context.Context, records []storage.Record, events []agent.Event) error {
	if w.opts.ReadOnly {
		return product.NewError(product.CodePermissionDenied, "workflow is read-only")
	}
	if w.closed {
		return product.NewError(product.CodeStateConflict, "workflow is closed")
	}
	if w.broken != nil {
		return w.broken
	}
	if w.state.RepairRequired {
		return product.NewError(product.CodeStorageUnavailable, "workflow journal requires repair")
	}
	next := w.state.clone()
	for _, r := range records {
		if err := next.apply(r); err != nil {
			return err
		}
	}
	if err := next.validateCombination(); err != nil {
		return err
	}
	commit := storage.Commit{RecordType: "commit", Version: 1, CommitID: agent.MustID(), CommitSeq: w.state.Revision + 1, ExpectedPreviousSeq: w.state.Revision, ControlRecords: records, Events: events}
	for i := range commit.Events {
		seq := w.state.Cursor + uint64(i) + 1
		commit.Events[i].DurableSeq = &seq
		if err := commit.Events[i].Validate(); err != nil {
			return err
		}
		next.Events = append(next.Events, commit.Events[i])
		next.Cursor = seq
	}
	receipt, err := w.store.Append(ctx, w.opts.RunID, storage.ExpectedCommit{ExpectedPreviousSeq: w.state.Revision}, storage.CloneCommit(commit))
	if err != nil {
		w.broken = product.NewError(product.CodeStorageUnavailable, "workflow commit failed")
		return w.broken
	}
	if receipt.CommitSeq != commit.CommitSeq || receipt.CommitID != commit.CommitID {
		w.broken = product.NewError(product.CodeStorageUnavailable, "workflow commit receipt is inconsistent")
		return w.broken
	}
	next.Revision = receipt.CommitSeq
	w.state = next
	for _, ev := range commit.Events {
		w.publishLocked(ev)
	}
	return nil
}
func (s runState) validateCombination() error {
	if (s.Run.State == "created") != (s.Input == nil) && s.Run.State != "cancelled" {
		return invalidRecord()
	}
	for _, n := range s.Nodes {
		definition, ok := s.definitionForNode(n)
		if !ok {
			return invalidRecord()
		}
		bound := false
		for _, declared := range definition.Nodes {
			if declared.ID == n.NodeID && declared.Type == n.Kind {
				bound = n.Kind != "tool" || s.Calls[n.ToolCallID].Call.Name == declared.Tool
			}
		}
		if !bound || n.Kind == "subflow" && n.ChildInvocationID == "" || n.Kind != "subflow" && n.ChildInvocationID != "" {
			return invalidRecord()
		}
		if n.ID != s.Initial.RunID+":"+n.InvocationID+":"+n.Path+":1" {
			return invalidRecord()
		}
		if n.Kind == "tool" {
			c, ok := s.Calls[n.ToolCallID]
			if !ok || c.Scope.NodeExecutionID != n.ID || c.Scope.InvocationID != n.InvocationID {
				return invalidRecord()
			}
		}
		if s.Run.State == "completed" && n.State != "completed" {
			return invalidRecord()
		}
	}
	if s.Run.State == "completed" {
		actual, err := canonicalInput(s.Run.Result)
		if err != nil {
			return invalidRecord()
		}
		ends := 0
		for _, end := range s.Initial.Manifest.Definition.Nodes {
			if end.Type != "end" {
				continue
			}
			ends++
			expected, err := s.projectValues(s.Run.InvocationID, s.Initial.Manifest.Definition, end.Inputs)
			if err != nil {
				return invalidRecord()
			}
			expected, err = canonicalInput(expected)
			if err != nil || !bytes.Equal(actual, expected) {
				return invalidRecord()
			}
		}
		if ends != 1 {
			return invalidRecord()
		}
	}
	if s.Run.ExecutionStopped {
		for _, a := range s.Attempts {
			if a.Status == "started" {
				return invalidRecord()
			}
		}
		for _, op := range s.Operations {
			if op.State == "running" {
				return invalidRecord()
			}
		}
	}
	return nil
}

func (s runState) definitionForNode(n NodeRun) (WorkflowDefinition, bool) {
	if n.InvocationID == s.Run.InvocationID {
		return s.Initial.Manifest.Definition, n.DefinitionHash == s.Initial.Manifest.DefinitionHash && n.Path == n.NodeID
	}
	for _, parent := range s.Nodes {
		if parent.Kind != "subflow" || parent.ChildInvocationID != n.InvocationID || n.Path != parent.Path+"/"+n.NodeID {
			continue
		}
		def := s.Initial.Manifest.Definition
		if parent.DefinitionHash != s.Initial.Manifest.DefinitionHash {
			found := false
			for key, hash := range s.Initial.Manifest.SubflowHashes {
				if hash == parent.DefinitionHash {
					def, found = s.Initial.Manifest.Subflows[key], true
					break
				}
			}
			if !found {
				return WorkflowDefinition{}, false
			}
		}
		for _, declared := range def.Nodes {
			if declared.ID == parent.NodeID && declared.Type == "subflow" && s.Initial.Manifest.SubflowHashes[declared.Subflow] == n.DefinitionHash {
				bound, ok := s.Initial.Manifest.Subflows[declared.Subflow]
				return bound, ok
			}
		}
	}
	return WorkflowDefinition{}, false
}

// projectValues checks only the static definition and already committed node
// outputs. It never invokes a graph, model, tool, or backend during replay.
func (s runState) projectValues(invocation string, definition WorkflowDefinition, values map[string]WorkflowValue) (json.RawMessage, error) {
	return s.projectValuesRemaining(invocation, definition, values, len(s.Nodes)+len(definition.Nodes)+config.SubagentDepth+1)
}
func (s runState) projectValuesRemaining(invocation string, definition WorkflowDefinition, values map[string]WorkflowValue, remaining int) (json.RawMessage, error) {
	if remaining <= 0 {
		return nil, invalidRecord()
	}
	out := map[string]json.RawMessage{}
	for key, value := range values {
		raw := value.Literal
		if value.Ref != nil {
			var err error
			raw, err = s.projectOutput(invocation, definition, value.Ref.Node, value.Ref.Field, remaining-1)
			if err != nil {
				return nil, err
			}
		}
		if !json.Valid(raw) {
			return nil, invalidRecord()
		}
		out[key] = raw
	}
	raw, err := json.Marshal(out)
	return raw, err
}
func (s runState) projectOutput(invocation string, definition WorkflowDefinition, nodeID, field string, remaining int) (json.RawMessage, error) {
	if remaining <= 0 {
		return nil, invalidRecord()
	}
	for _, declared := range definition.Nodes {
		if declared.ID != nodeID {
			continue
		}
		var raw json.RawMessage
		switch declared.Type {
		case "start":
			if invocation == s.Run.InvocationID {
				raw = s.Input.Input
			} else {
				for _, parent := range s.Nodes {
					if parent.ChildInvocationID != invocation {
						continue
					}
					outer, ok := s.definitionForNode(parent)
					if !ok {
						return nil, invalidRecord()
					}
					for _, d := range outer.Nodes {
						if d.ID == parent.NodeID {
							var err error
							raw, err = s.projectValuesRemaining(parent.InvocationID, outer, d.Inputs, remaining-1)
							if err != nil {
								return nil, err
							}
						}
					}
				}
			}
		case "literal":
			var err error
			raw, err = s.projectValuesRemaining(invocation, definition, declared.Inputs, remaining-1)
			if err != nil {
				return nil, err
			}
		case "model", "tool", "subflow":
			for _, n := range s.Nodes {
				if n.InvocationID == invocation && n.NodeID == nodeID && n.State == "completed" {
					if declared.Type == "subflow" {
						raw = json.RawMessage(n.Result)
					} else if field == declared.OutputField() {
						return json.Marshal(n.Result)
					}
				}
			}
		}
		var values map[string]json.RawMessage
		if json.Unmarshal(raw, &values) == nil && values[field] != nil {
			return values[field], nil
		}
	}
	return nil, invalidRecord()
}

func (s runState) modelForNode(n NodeRun) (modelBinding, bool) {
	definition, ok := s.definitionForNode(n)
	if !ok {
		return modelBinding{}, false
	}
	for _, declared := range definition.Nodes {
		if declared.ID == n.NodeID && declared.Type == "model" {
			for _, binding := range s.Initial.Manifest.Models {
				if binding.Name == declared.Model {
					return binding, true
				}
			}
		}
	}
	return modelBinding{}, false
}

func normalizePolicy(p *agent.ResolvedPolicy) (agent.ResolvedPolicy, error) {
	v := agent.ResolvedPolicy{}
	if p != nil {
		v = *p
	}
	v.Ref = ""
	v.Revision = 1
	if v.SandboxMode == "" {
		v.SandboxMode = "workspace-write"
	}
	if v.ApprovalPolicy == "" {
		v.ApprovalPolicy = "ask"
	}
	switch v.SandboxMode {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return v, product.NewError(product.CodeInvalidArgument, "invalid execution sandbox mode")
	}
	if v.ApprovalPolicy != "ask" && v.ApprovalPolicy != "never" {
		return v, product.NewError(product.CodeInvalidArgument, "invalid execution approval policy")
	}
	if v.Auto {
		return v, product.NewError(product.CodeUnsupportedCapability, "automatic execution review is unavailable")
	}
	v.Ref = digest(struct {
		Revision       uint64 `json:"revision"`
		SandboxMode    string `json:"sandboxMode"`
		ApprovalPolicy string `json:"approvalPolicy"`
		Auto           bool   `json:"auto"`
	}{v.Revision, v.SandboxMode, v.ApprovalPolicy, v.Auto})
	return v, nil
}
