package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/agent"
	"github.com/ww1489/seasprak/model"
)

type TraceState struct {
	ID           string            `json:"id"`
	State        string            `json:"state"`
	Kind         string            `json:"kind"`
	Target       agent.TargetAgent `json:"target"`
	Generation   string            `json:"generation"`
	Settled      bool              `json:"settled"`
	Hold         bool              `json:"hold"`
	Started      bool              `json:"started"`
	InvocationID string            `json:"invocationId,omitempty"`
	Limits       model.Limits      `json:"limits"`
	Usage        agent.Usage       `json:"usage"`
	HoldOnStop   []string          `json:"holdOnStop,omitempty"`
	Error        string            `json:"error,omitempty"`
}
type InputState struct {
	ID        string            `json:"id"`
	TraceID   string            `json:"traceId"`
	Kind      string            `json:"kind"`
	State     string            `json:"state"`
	Content   json.RawMessage   `json:"content"`
	Principal string            `json:"principal"`
	Target    agent.TargetAgent `json:"target"`
	CommitSeq uint64            `json:"commitSeq"`
}
type View struct {
	LastSeq        uint64
	Cursor         uint64
	BranchID       string
	LeafID         string
	Traces         map[string]*TraceState
	Inputs         map[string]*InputState
	Order          []string
	Steering       []string
	Follow         []string
	Independent    []string
	Idem           map[string]idemRecord
	Budget         agent.Usage // compatibility: aggregate diagnostic; limits are per Trace
	Generation     string
	Events         []agent.Event
	ActiveTrace    string
	Messages       []agent.AgentMessage
	Turns          map[string]agent.TurnRecord
	Calls          map[string]agent.ToolRecord
	RepairRequired bool
}
type idemRecord struct {
	Digest  string
	Receipt agent.InputReceipt
}
type Manager struct {
	mu        sync.Mutex
	store     SessionStore
	sessionID string
	view      *View
	fault     error
}

func NewManager(store SessionStore, id string) (*Manager, error) {
	stored, err := store.Load(context.Background(), id)
	if err != nil {
		return nil, err
	}
	v := emptyView()
	v.RepairRequired = stored.RepairRequired
	for _, c := range stored.Commits {
		if err := applyCommit(v, c); err != nil {
			return nil, err
		}
	}
	return &Manager{store: store, sessionID: id, view: v}, nil
}
func emptyView() *View {
	return &View{BranchID: "main", Traces: map[string]*TraceState{}, Inputs: map[string]*InputState{}, Idem: map[string]idemRecord{}, Turns: map[string]agent.TurnRecord{}, Calls: map[string]agent.ToolRecord{}}
}
func clone[T any](v T) T {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}
func (m *Manager) View() View   { m.mu.Lock(); defer m.mu.Unlock(); return clone(*m.view) }
func (m *Manager) Fault() error { m.mu.Lock(); defer m.mu.Unlock(); return m.fault }
func record(kind, id string, v any) Record {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return Record{Type: kind, Version: 1, ID: id, Payload: raw}
}
func (m *Manager) event(kind, trace, turn string, v any) agent.Event {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return agent.Event{SchemaVersion: 1, Type: kind, Scope: agent.EventScope{SessionID: m.sessionID, TraceID: trace, TurnID: turn}, EventID: agent.MustID(), OccurredAt: time.Now().UTC(), Payload: raw}
}

// commit validates a private candidate. Neither state nor events become visible before Append succeeds.
func (m *Manager) commit(ctx context.Context, controls, entries []Record, events []agent.Event) (CommitReceipt, error) {
	if m.fault != nil {
		return CommitReceipt{}, m.fault
	}
	if m.view.RepairRequired {
		return CommitReceipt{}, model.NewError(model.CodeStorageUnavailable, "journal requires repair")
	}
	if err := ctx.Err(); err != nil {
		return CommitReceipt{}, err
	}
	c := Commit{RecordType: "commit", Version: 1, CommitID: agent.MustID(), CommitSeq: m.view.LastSeq + 1, ExpectedPreviousSeq: m.view.LastSeq, ControlRecords: controls, Entries: entries, Events: events}
	for i := range c.Events {
		seq := m.view.Cursor + uint64(i) + 1
		c.Events[i].DurableSeq = &seq
	}
	candidate := clone(*m.view)
	if err := applyCommit(&candidate, c); err != nil {
		return CommitReceipt{}, err
	}
	receipt, err := m.store.Append(ctx, m.sessionID, ExpectedCommit{ExpectedPreviousSeq: m.view.LastSeq}, c)
	if err != nil {
		m.fault = model.NewError(model.CodeStorageUnavailable, "session commit failed; reopen required")
		return CommitReceipt{}, err
	}
	m.view = &candidate
	return receipt, nil
}
func (m *Manager) Accept(ctx context.Context, cmd agent.InputCommand, target agent.TargetAgent) (agent.InputReceipt, error) {
	return m.AcceptWithLimits(ctx, cmd, target, model.DefaultLimits())
}
func (m *Manager) AcceptWithLimits(ctx context.Context, cmd agent.InputCommand, target agent.TargetAgent, limits model.Limits) (agent.InputReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	digest, err := digestCommand(cmd)
	if err != nil {
		return agent.InputReceipt{}, err
	}
	keyBytes, _ := json.Marshal([]string{cmd.Principal, "input", cmd.IdempotencyKey})
	key := string(keyBytes)
	if cmd.IdempotencyKey != "" {
		if old, ok := m.view.Idem[key]; ok {
			if old.Digest != digest {
				return agent.InputReceipt{}, model.NewError(model.CodeIdempotencyConflict, "key already belongs to different content")
			}
			return old.Receipt, nil
		}
	}
	kind, traceID, err := m.classify(cmd, target)
	if err != nil {
		return agent.InputReceipt{}, err
	}
	var trace TraceState
	if traceID == "" {
		traceID = agent.MustID()
		trace = TraceState{ID: traceID, State: "queued", Kind: kind, Target: target, Generation: target.Generation, Limits: limits.WithDefaults()}
	} else {
		trace = *m.view.Traces[traceID]
		target = trace.Target
	}
	id := agent.MustID()
	seq := m.view.LastSeq + 1
	input := InputState{ID: id, TraceID: traceID, Kind: kind, State: "pending", Content: append(json.RawMessage(nil), cmd.Content...), Principal: cmd.Principal, Target: target, CommitSeq: seq}
	receipt := agent.InputReceipt{InputID: id, TraceID: traceID, ActualKind: kind, TargetAgent: target, State: "pending", AcceptedCommit: seq}
	controls := []Record{record("input", id, input)}
	if m.view.Traces[traceID] == nil {
		controls = append(controls, record("trace", traceID, trace))
	}
	if cmd.IdempotencyKey != "" {
		controls = append(controls, record("idempotency", key, idemRecord{Digest: digest, Receipt: receipt}))
	}
	_, err = m.commit(ctx, controls, nil, []agent.Event{m.event("input.accepted", traceID, "", receipt)})
	return receipt, err
}
func (m *Manager) classify(cmd agent.InputCommand, target agent.TargetAgent) (string, string, error) {
	if len(cmd.Content) == 0 || !json.Valid(cmd.Content) {
		return "", "", model.NewError(model.CodeInvalidArgument, "valid JSON content is required")
	}
	switch cmd.Kind {
	case "prompt", "chat":
		if cmd.TargetTraceID != "" {
			return "", "", model.NewError(model.CodeInvalidArgument, "independent input cannot target a trace")
		}
		if target.Name != "main" {
			return "", "", model.NewError(model.CodeUnsupportedCapability, "agent is not registered")
		}
		if cmd.Kind == "chat" && m.view.ActiveTrace != "" {
			tr := m.view.Traces[m.view.ActiveTrace]
			if tr.State != "running" {
				return "", "", model.NewError(model.CodeStateConflict, "active trace cannot accept chat")
			}
			return "follow_up", tr.ID, nil
		}
		return "prompt", "", nil
	case "steering", "follow_up":
		tr := m.view.Traces[cmd.TargetTraceID]
		if cmd.TargetTraceID == "" {
			return "", "", model.NewError(model.CodeInvalidArgument, "targetTraceId is required")
		}
		if tr == nil {
			return "", "", model.NewError(model.CodeNotFound, "trace not found")
		}
		if terminal(tr.State) || tr.State == "cancelling" || (cmd.Kind == "steering" && tr.State != "running") {
			return "", "", model.NewError(model.CodeStateConflict, "trace cannot accept directed input")
		}
		if cmd.TargetAgent != "" && cmd.TargetAgent != tr.Target.Name {
			return "", "", model.NewError(model.CodeStateConflict, "target mismatch")
		}
		return cmd.Kind, tr.ID, nil
	}
	return "", "", model.NewError(model.CodeInvalidArgument, "unknown input kind")
}
func (m *Manager) Consume(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := m.view.Inputs[id]
	if in == nil {
		return model.NewError(model.CodeNotFound, "input not found")
	}
	if in.State == "consumed" {
		return nil
	}
	if in.State != "pending" {
		return model.NewError(model.CodeStateConflict, "input is not pending")
	}
	next := *in
	next.State = "consumed"
	text := string(in.Content)
	var body struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(in.Content, &body) == nil && body.Text != "" {
		text = body.Text
	}
	msg := agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindUser, Status: agent.StatusComplete, Scope: agent.MessageScope{SessionID: m.sessionID, TraceID: in.TraceID, InputID: id}, Source: agent.SourceRef{Kind: agent.SourceHuman}, Standard: schema.UserAgenticMessage(text)}
	entry := record("message", msg.ID, msg)
	entry.ParentID = m.view.LeafID
	_, err := m.commit(ctx, []Record{record("input", id, next)}, []Record{entry}, []agent.Event{m.event("input.consumed", in.TraceID, "", next), m.event("message.finalized", in.TraceID, "", msg)})
	return err
}
func terminal(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled"
}
func (m *Manager) SetTraceState(ctx context.Context, id, state string, settled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.view.Traces[id]
	if old == nil {
		return model.NewError(model.CodeNotFound, "trace not found")
	}
	if old.State == state {
		return nil
	}
	if terminal(old.State) {
		return model.NewError(model.CodeStateConflict, "terminal trace cannot change")
	}
	allowed := false
	switch old.State {
	case "queued":
		allowed = state == "running" || state == "cancelled" || state == "failed"
	case "running":
		allowed = state == "completed" || state == "failed" || state == "paused" || state == "cancelling"
	case "cancelling":
		allowed = state == "cancelled" || state == "paused" || state == "failed"
	case "paused":
		allowed = state == "cancelling" || state == "failed"
	}
	if !allowed {
		return model.NewError(model.CodeStateConflict, "invalid trace transition")
	}
	if state == "running" {
		if old.State != "queued" || m.view.ActiveTrace != "" {
			return model.NewError(model.CodeStateConflict, "top-level execution is occupied")
		}
	}
	tr := *old
	tr.State = state
	if state == "running" {
		tr.Started = true
		tr.InvocationID = agent.MustID()
	}
	tr.Settled = terminal(state) && tr.Started
	if state == "cancelling" {
		for _, inID := range m.view.Independent {
			q := m.view.Inputs[inID]
			other := m.view.Traces[q.TraceID]
			if other.ID != id && other.State == "queued" {
				tr.HoldOnStop = append(tr.HoldOnStop, other.ID)
			}
		}
	}
	controls := []Record{record("trace", id, tr)}
	events := []agent.Event{m.event("trace.state_changed", id, "", tr)}
	if terminal(state) {
		if tr.Started {
			events = append(events, m.event("trace.settled", id, "", tr))
		}
		for _, inID := range m.view.Order {
			in := m.view.Inputs[inID]
			if in.TraceID == id && in.State == "pending" {
				next := *in
				next.State = "undelivered"
				controls = append(controls, record("input", in.ID, next))
			}
		}
		if state == "failed" {
			for _, inID := range m.view.Independent {
				other := m.view.Traces[m.view.Inputs[inID].TraceID]
				if other.ID != id && other.State == "queued" {
					tr.HoldOnStop = append(tr.HoldOnStop, other.ID)
				}
			}
		}
		for _, heldID := range tr.HoldOnStop {
			if other := m.view.Traces[heldID]; other != nil && other.State == "queued" {
				next := *other
				next.Hold = true
				controls = append(controls, record("trace", next.ID, next))
			}
		}
		events = append(events, m.event("queue.changed", id, "", map[string]string{"reason": state}))
	}
	_, err := m.commit(ctx, controls, nil, events)
	return err
}
func (m *Manager) HoldIndependent(ctx context.Context, id string) error {
	return m.setHold(ctx, id, true)
}
func (m *Manager) ReleaseHold(ctx context.Context, id string) error { return m.setHold(ctx, id, false) }
func (m *Manager) setHold(ctx context.Context, id string, hold bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[id]
	if tr == nil || tr.State != "queued" {
		return model.NewError(model.CodeStateConflict, "only queued trace can change hold")
	}
	next := *tr
	next.Hold = hold
	_, err := m.commit(ctx, []Record{record("trace", id, next)}, nil, []agent.Event{m.event("queue.changed", id, "", next)})
	return err
}
func (m *Manager) SaveTraceError(ctx context.Context, id, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[id]
	if tr == nil {
		return model.NewError(model.CodeNotFound, "trace not found")
	}
	next := *tr
	next.Error = message
	_, err := m.commit(ctx, []Record{record("trace", id, next)}, nil, nil)
	return err
}
func (m *Manager) SaveTraceBudget(ctx context.Context, id string, usage agent.Usage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr := m.view.Traces[id]
	if tr == nil {
		return model.NewError(model.CodeNotFound, "trace not found")
	}
	next := *tr
	next.Usage = usage
	_, err := m.commit(ctx, []Record{record("trace", id, next)}, nil, nil)
	return err
}
func (m *Manager) SaveBudget(ctx context.Context, usage agent.Usage) error {
	v := m.View()
	if v.ActiveTrace == "" {
		return model.NewError(model.CodeStateConflict, "no active trace")
	}
	return m.SaveTraceBudget(ctx, v.ActiveTrace, usage)
}
func (m *Manager) AppendEvent(ctx context.Context, kind, trace string, payload []byte) (CommitReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commit(ctx, nil, nil, []agent.Event{m.event(kind, trace, "", json.RawMessage(payload))})
}
func (m *Manager) AppendMessage(ctx context.Context, msg agent.AgentMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := msg.Validate(); err != nil {
		return err
	}
	entry := record("message", msg.ID, msg)
	entry.ParentID = m.view.LeafID
	_, err := m.commit(ctx, nil, []Record{entry}, []agent.Event{m.event("message.finalized", msg.Scope.TraceID, msg.Scope.TurnID, msg)})
	return err
}
func (m *Manager) SaveTurn(ctx context.Context, tr agent.TurnRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.view.Turns[tr.ID]; ok && old.Ended {
		return nil
	}
	kind := "turn_start"
	if tr.Ended {
		kind = "turn_end"
	}
	_, err := m.commit(ctx, []Record{record("turn", tr.ID, tr)}, nil, []agent.Event{m.event(kind, tr.TraceID, tr.ID, tr)})
	return err
}
func (m *Manager) SaveAssistant(ctx context.Context, msg agent.AgentMessage, calls []agent.ToolRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := msg.Validate(); err != nil {
		return err
	}
	turn, ok := m.view.Turns[msg.Scope.TurnID]
	if !ok || turn.Ended || turn.TraceID != msg.Scope.TraceID {
		return model.NewError(model.CodeStateConflict, "assistant turn is not active")
	}
	var controls []Record
	events := []agent.Event{m.event("message.finalized", msg.Scope.TraceID, msg.Scope.TurnID, msg)}
	for _, call := range calls {
		controls = append(controls, record("tool_call", call.Call.CallID, call))
		turn.CallIDs = append(turn.CallIDs, call.Call.CallID)
		events = append(events, m.event("tool.requested", msg.Scope.TraceID, msg.Scope.TurnID, call))
	}
	controls = append(controls, record("turn", turn.ID, turn))
	entry := record("message", msg.ID, msg)
	entry.ParentID = m.view.LeafID
	_, err := m.commit(ctx, controls, []Record{entry}, events)
	return err
}
func (m *Manager) SaveCall(ctx context.Context, call agent.ToolRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.view.Calls[call.Call.CallID]
	if !ok {
		return model.NewError(model.CodeStateConflict, "tool call was not accepted")
	}
	if old.Call != call.Call || old.Scope != call.Scope || (old.Claimed && !call.Claimed) {
		return model.NewError(model.CodeStateConflict, "accepted tool identity is immutable")
	}
	if old.Observation != nil {
		if call.Observation == nil || *old.Observation != *call.Observation {
			return model.NewError(model.CodeStateConflict, "tool result already recorded")
		}
		return nil
	}
	kind := "tool.requested"
	if call.Claimed {
		kind = "tool.state_changed"
	}
	if call.Observation != nil {
		kind = "tool.finished"
	}
	_, err := m.commit(ctx, []Record{record("tool_call", call.Call.CallID, call)}, nil, []agent.Event{m.event(kind, call.Scope.TraceID, call.Scope.TurnID, call)})
	return err
}

// FinishTools persists results in the original assistant call order, independent of completion order.
func (m *Manager) FinishTools(ctx context.Context, turn agent.TurnRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.view.Turns[turn.ID]
	if old.Ended {
		return nil
	}
	var entries []Record
	parent := m.view.LeafID
	for _, id := range turn.CallIDs {
		call, ok := m.view.Calls[id]
		if !ok || call.Observation == nil {
			return model.NewError(model.CodeReconciliationRequired, "tool result is unresolved")
		}
		result := &schema.FunctionToolResult{CallID: call.Call.ProviderCallID, Name: call.Call.Name, Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: call.Observation.Content}}}}
		msg := agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindToolResult, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceTool}, Scope: agent.MessageScope{SessionID: m.sessionID, TraceID: turn.TraceID, TurnID: turn.ID, InvocationID: turn.InvocationID, ToolCallID: id}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(result)}}}
		entry := record("message", msg.ID, msg)
		entry.ParentID = parent
		parent = entry.ID
		entries = append(entries, entry)
	}
	turn.Ended = true
	events := []agent.Event{}
	for _, entry := range entries {
		events = append(events, m.event("message.finalized", turn.TraceID, turn.ID, entry.Payload))
	}
	events = append(events, m.event("turn_end", turn.TraceID, turn.ID, turn))
	_, err := m.commit(ctx, []Record{record("turn", turn.ID, turn)}, entries, events)
	return err
}
func applyCommit(v *View, c Commit) error {
	for _, r := range c.ControlRecords {
		if err := applyControl(v, r); err != nil {
			return err
		}
	}
	for _, r := range c.Entries {
		if r.Type != "message" {
			return model.NewError(model.CodeIncompatibleVersion, "unknown required entry")
		}
		var msg agent.AgentMessage
		if err := json.Unmarshal(r.Payload, &msg); err != nil {
			return err
		}
		if err := msg.Validate(); err != nil {
			return err
		}
		if r.ParentID != v.LeafID {
			return model.NewError(model.CodeIncompatibleVersion, "history parent is not the selected leaf")
		}
		v.Messages = append(v.Messages, msg)
		v.LeafID = r.ID
	}
	v.LastSeq = c.CommitSeq
	for _, ev := range c.Events {
		if ev.DurableSeq != nil {
			v.Cursor = *ev.DurableSeq
		}
		v.Events = append(v.Events, ev)
	}
	v.ActiveTrace = ""
	for id, tr := range v.Traces {
		if tr.Started && !terminal(tr.State) {
			if v.ActiveTrace != "" && v.ActiveTrace != id {
				return model.NewError(model.CodeIncompatibleVersion, "multiple active traces")
			}
			v.ActiveTrace = id
		}
	}
	return nil
}
func applyControl(v *View, r Record) error {
	if r.Version != 1 {
		return model.NewError(model.CodeIncompatibleVersion, "unknown control version")
	}
	switch r.Type {
	case "input":
		var in InputState
		if err := json.Unmarshal(r.Payload, &in); err != nil {
			return err
		}
		if in.ID != r.ID {
			return fmt.Errorf("input identity mismatch")
		}
		if v.Inputs[in.ID] == nil {
			v.Order = append(v.Order, in.ID)
			switch in.Kind {
			case "steering":
				v.Steering = append(v.Steering, in.ID)
			case "follow_up":
				v.Follow = append(v.Follow, in.ID)
			default:
				v.Independent = append(v.Independent, in.ID)
			}
		}
		v.Inputs[in.ID] = &in
	case "trace", "generation_ref", "queue_hold":
		var tr TraceState
		if err := json.Unmarshal(r.Payload, &tr); err != nil {
			return err
		}
		v.Traces[tr.ID] = &tr
		v.Generation = tr.Generation
	case "idempotency":
		var item idemRecord
		if err := json.Unmarshal(r.Payload, &item); err != nil {
			return err
		}
		v.Idem[r.ID] = item
	case "turn":
		var tr agent.TurnRecord
		if err := json.Unmarshal(r.Payload, &tr); err != nil {
			return err
		}
		v.Turns[tr.ID] = tr
	case "tool_call":
		var call agent.ToolRecord
		if err := json.Unmarshal(r.Payload, &call); err != nil {
			return err
		}
		v.Calls[call.Call.CallID] = call
	case "budget":
		return json.Unmarshal(r.Payload, &v.Budget)
	case "active_cursor":
		var cursor struct{ BranchID, LeafID string }
		if err := json.Unmarshal(r.Payload, &cursor); err != nil {
			return err
		}
		v.BranchID = cursor.BranchID
		v.LeafID = cursor.LeafID
	default:
		return model.NewError(model.CodeIncompatibleVersion, "unknown required control record")
	}
	return nil
}
func digestCommand(cmd agent.InputCommand) (string, error) {
	if !json.Valid(cmd.Content) {
		return "", model.NewError(model.CodeInvalidArgument, "invalid input content")
	}
	dec := json.NewDecoder(bytes.NewReader(cmd.Content))
	dec.UseNumber()
	var content any
	if err := dec.Decode(&content); err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Kind, TargetTraceID, TargetAgent string
		Content                          any
	}{cmd.Kind, cmd.TargetTraceID, cmd.TargetAgent, content})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}
