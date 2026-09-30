package web

import (
	"cmp"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

// uiState is the per-stream memory of what the A2UI surface shows. It is
// seeded from a consistent snapshot so root children stay complete when a
// stream starts at any cursor; entries only grow or overwrite by stable ID.
type uiState struct {
	sid        string
	seedCursor uint64
	order      []string // message section: msg:{id}, each followed by its call:{id}
	tasks      []string
	approvals  []string
	messages   map[string]*uiMessage
	texts      map[string]string
	calls      map[string]*a2uiToolCall
	callOwner  map[string]string // product call ID -> owning message ID
	provider   map[string]string // turnID \x00 providerCallID -> message ID
	traces     map[string]*a2uiTask
	approval   map[string]*a2uiApproval
	outputs    map[string]string
	// progress lists Invocation then WorkflowNode children, each sorted by
	// product ID; they have no durable events and refresh from snapshots.
	progress []string
	progComp map[string]a2uiComponent
}

type uiMessage struct {
	role, status, traceID string
}

func isTerminal(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled"
}

func msgID(id string) string     { return "msg:" + id }
func callID(id string) string    { return "call:" + id }
func taskID(id string) string    { return "task:" + id }
func apprID(id string) string    { return "approval:" + id }
func textKey(id string) string   { return "text:" + id }
func outputKey(id string) string { return "output:" + id }

// messageRole maps product message kinds to display roles; commands, opaque
// records and hidden custom messages are not rendered.
func messageRole(m agent.AgentMessage) string {
	switch m.Kind {
	case agent.KindUser:
		return "user"
	case agent.KindAssistant:
		return "assistant"
	case agent.KindToolResult:
		return "tool"
	case agent.KindCompactionSummary, agent.KindBranchSummary:
		return "summary"
	case agent.KindCustom:
		if m.Custom != nil && m.Custom.Display {
			return "assistant"
		}
	}
	return ""
}

// messageText joins public text; projectMessage already drops reasoning,
// signatures and provider extensions.
func messageText(dto messageDTO) string {
	var parts []string
	for _, b := range dto.Blocks {
		if (b.Type == "text" || b.Type == "tool_result") && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// streamText joins visible assistant text. The Eino adapter reports text
// blocks with their schema type "assistant_gen_text"; "text" is kept for
// producers that already use the public block name. Reasoning never matches.
func streamText(s agent.ModelStreamSnapshot) string {
	var b strings.Builder
	for _, blk := range s.Blocks {
		if blk.Type == "text" || blk.Type == string(schema.ContentBlockTypeAssistantGenText) {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

func toolStatus(rec agent.ToolRecord) string {
	switch {
	case rec.Observation != nil:
		return rec.Observation.Status
	case rec.Claimed:
		return "running"
	}
	return "requested"
}

// boundPreview keeps the newest bytes of a tool preview at a UTF-8 boundary,
// matching the session-side transient limit.
func boundPreview(s string) string {
	n := config.TransientToolPreviewBytes
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

func newUIState(sid string, snap sessions.Snapshot, instanceID string) *uiState {
	st := &uiState{sid: sid, seedCursor: snap.Cursor, messages: map[string]*uiMessage{}, texts: map[string]string{}, calls: map[string]*a2uiToolCall{}, callOwner: map[string]string{}, provider: map[string]string{}, traces: map[string]*a2uiTask{}, approval: map[string]*a2uiApproval{}, outputs: map[string]string{}}
	byProvider := map[string]string{}
	for id, rec := range snap.Calls {
		byProvider[rec.Scope.TurnID+"\x00"+rec.Call.ProviderCallID] = id
	}
	for _, m := range snap.Messages {
		role := messageRole(m)
		if role == "" {
			continue
		}
		dto := projectMessage(m)
		st.messages[m.ID] = &uiMessage{role: role, status: "final", traceID: m.Scope.TraceID}
		st.texts[m.ID] = messageText(dto)
		st.order = append(st.order, msgID(m.ID))
		for _, b := range dto.Blocks {
			if b.Type != "tool_call" {
				continue
			}
			key := m.Scope.TurnID + "\x00" + b.CallID
			st.provider[key] = m.ID
			if pid, ok := byProvider[key]; ok && st.calls[pid] == nil {
				rec := snap.Calls[pid]
				st.calls[pid] = &a2uiToolCall{CallID: pid, Name: rec.Call.Name, Status: toolStatus(rec)}
				st.callOwner[pid] = m.ID
				st.order = append(st.order, callID(pid))
			}
		}
	}
	// Tasks follow first acceptance order; sorted trace IDs break ties.
	first := map[string]uint64{}
	for _, in := range snap.Inputs {
		if in != nil && (first[in.TraceID] == 0 || in.CommitSeq < first[in.TraceID]) {
			first[in.TraceID] = in.CommitSeq
		}
	}
	ids := sortedKeys(snap.Traces)
	slices.SortStableFunc(ids, func(a, b string) int { return cmp.Compare(first[a], first[b]) })
	for _, id := range ids {
		if tr := snap.Traces[id]; tr != nil {
			st.traces[id] = &a2uiTask{TraceID: id, State: tr.State, TargetAgent: tr.Target.Name, Settled: tr.Settled}
			st.tasks = append(st.tasks, taskID(id))
		}
	}
	for _, k := range sortedKeys(snap.Transient.Models) {
		m := snap.Transient.Models[k]
		mid := m.Snapshot.MessageID
		if mid == "" || st.messages[mid] != nil || st.traceDone(m.TraceID) {
			continue
		}
		st.messages[mid] = &uiMessage{role: "assistant", status: "streaming", traceID: m.TraceID}
		st.texts[mid] = streamText(m.Snapshot)
		st.order = append(st.order, msgID(mid))
	}
	for _, k := range sortedKeys(snap.Transient.Tools) {
		o := snap.Transient.Tools[k]
		if st.calls[o.ToolCallID] != nil && !st.traceDone(o.TraceID) {
			st.outputs[o.ToolCallID] = boundPreview(st.outputs[o.ToolCallID] + o.Text)
		}
	}
	st.syncProgress(snap)
	st.syncApprovals(snap, instanceID)
	return st
}

// syncProgress replaces the Invocation and WorkflowNode components with the
// snapshot's records and reports whether any visible component changed.
func (st *uiState) syncProgress(snap sessions.Snapshot) bool {
	next := map[string]a2uiComponent{}
	var order []string
	for _, id := range sortedKeys(snap.Invocations) {
		c := invocationComponent(snap.Invocations[id])
		next[c.ID], order = c, append(order, c.ID)
	}
	for _, id := range sortedKeys(snap.WorkflowNodes) {
		c := workflowNodeComponent(snap.WorkflowNodes[id])
		next[c.ID], order = c, append(order, c.ID)
	}
	changed := !slices.Equal(order, st.progress)
	for _, id := range order {
		if old, ok := st.progComp[id]; !ok || !reflect.DeepEqual(old, next[id]) {
			changed = true
		}
	}
	st.progress, st.progComp = order, next
	return changed
}

// progressFrames returns the temporary frames showing the current progress set.
func (st *uiState) progressFrames() []a2uiMessage {
	comps := make([]a2uiComponent, 0, len(st.progress)+1)
	for _, id := range st.progress {
		comps = append(comps, st.progComp[id])
	}
	return []a2uiMessage{surfaceUpdate(st.sid, append(comps, st.root())...)}
}

func (st *uiState) traceDone(traceID string) bool {
	tr := st.traces[traceID]
	return tr != nil && isTerminal(tr.State)
}

// syncApprovals replaces the approval set with the pending runtime approvals
// of this instance and reports whether the visible set changed. Only the
// product interactionId is exposed, never framework interrupt addresses.
func (st *uiState) syncApprovals(snap sessions.Snapshot, instanceID string) bool {
	next := map[string]*a2uiApproval{}
	var order []string
	if instanceID != "" {
		for _, id := range sortedKeys(snap.Interactions) {
			in := snap.Interactions[id]
			if in.Kind != "approval" || in.State != "ready" || in.ID == "" {
				continue
			}
			next[in.ID] = &a2uiApproval{InteractionID: in.ID, TraceID: in.Scope.TraceID, Question: in.Question, Options: append([]string{}, in.Options...), InstanceID: instanceID}
			order = append(order, apprID(in.ID))
		}
	}
	changed := !slices.Equal(order, st.approvals)
	st.approval, st.approvals = next, order
	return changed
}

func (st *uiState) root() a2uiComponent {
	children := make([]string, 0, len(st.order)+len(st.tasks)+len(st.progress)+len(st.approvals))
	children = append(append(append(append(children, st.order...), st.tasks...), st.progress...), st.approvals...)
	return a2uiComponent{ID: a2uiRootID, Component: a2uiComponentValue{Column: &a2uiChildren{Children: children}}}
}

func (st *uiState) messageComponent(id string) a2uiComponent {
	m := st.messages[id]
	return a2uiComponent{ID: msgID(id), Component: a2uiComponentValue{ChatMessage: &a2uiChatMessage{MessageID: id, Role: m.role, Status: m.status, DataKey: textKey(id)}}}
}

func (st *uiState) callComponent(id string) a2uiComponent {
	c := *st.calls[id]
	return a2uiComponent{ID: callID(id), Component: a2uiComponentValue{ToolCall: &c}}
}

func (st *uiState) taskComponent(id string) a2uiComponent {
	t := *st.traces[id]
	return a2uiComponent{ID: taskID(id), Component: a2uiComponentValue{Task: &t}}
}

func invID(id string) string  { return "inv:" + id }
func nodeID(id string) string { return "node:" + id }

// invocationComponent projects identity, target name and state of one child
// invocation; its result text never leaves the session.
func invocationComponent(inv state.Invocation) a2uiComponent {
	return a2uiComponent{ID: invID(inv.ID), Component: a2uiComponentValue{Invocation: &a2uiInvocation{InvocationID: inv.ID, ParentCallID: inv.ParentCallID, Agent: inv.Target.Name, State: inv.State}}}
}

// workflowNodeComponent projects identity, node, kind and state of one node
// execution; node results and error text stay private.
func workflowNodeComponent(n state.WorkflowNodeRun) a2uiComponent {
	return a2uiComponent{ID: nodeID(n.ID), Component: a2uiComponentValue{WorkflowNode: &a2uiWorkflowNode{NodeExecutionID: n.ID, TraceID: n.TraceID, NodeID: n.NodeID, Kind: n.Kind, State: n.State}}}
}

func (st *uiState) approvalComponent(id string) a2uiComponent {
	a := *st.approval[id]
	a.Options = append([]string{}, a.Options...)
	return a2uiComponent{ID: apprID(id), Component: a2uiComponentValue{Approval: &a}}
}

// renderSnapshot rebuilds the complete surface of one consistent snapshot:
// beginRendering, every component plus root, then the bound texts.
func renderSnapshot(sid string, snap sessions.Snapshot, instanceID string) []a2uiMessage {
	return newUIState(sid, snap, instanceID).render()
}

func (st *uiState) render() []a2uiMessage {
	out := []a2uiMessage{{BeginRendering: &a2uiBeginRendering{SurfaceID: a2uiSurfaceID(st.sid), Root: a2uiRootID}}}
	var comps []a2uiComponent
	var contents []a2uiContent
	for _, child := range st.root().Component.Column.Children {
		kind, id, _ := strings.Cut(child, ":")
		switch kind {
		case "msg":
			comps = append(comps, st.messageComponent(id))
			contents = append(contents, a2uiContent{Key: textKey(id), ValueString: st.texts[id]})
		case "call":
			comps = append(comps, st.callComponent(id))
			if text, ok := st.outputs[id]; ok {
				contents = append(contents, a2uiContent{Key: outputKey(id), ValueString: text})
			}
		case "task":
			comps = append(comps, st.taskComponent(id))
		case "inv", "node":
			comps = append(comps, st.progComp[child])
		case "approval":
			comps = append(comps, st.approvalComponent(id))
		}
	}
	out = append(out, surfaceUpdate(st.sid, append(comps, st.root())...))
	if len(contents) > 0 {
		out = append(out, dataUpdate(st.sid, contents...))
	}
	return out
}

// approvalFrames returns the temporary frames showing the current approval set.
func (st *uiState) approvalFrames() []a2uiMessage {
	comps := make([]a2uiComponent, 0, len(st.approvals)+1)
	for _, child := range st.approvals {
		comps = append(comps, st.approvalComponent(strings.TrimPrefix(child, "approval:")))
	}
	return []a2uiMessage{surfaceUpdate(st.sid, append(comps, st.root())...)}
}

// insertCall places a call after its owning message and that message's
// earlier calls; an unknown owner appends it to the message section.
func (st *uiState) insertCall(id, owner string) {
	at := len(st.order)
	if owner != "" {
		if i := slices.Index(st.order, msgID(owner)); i >= 0 {
			at = i + 1
			for at < len(st.order) && strings.HasPrefix(st.order[at], "call:") {
				at++
			}
		}
	}
	st.order = slices.Insert(st.order, at, callID(id))
	st.callOwner[id] = owner
}

// projectUIEvent converts one product event into A2UI frames and updates
// known. Durable events at or before the seed cursor are already reflected in
// known, so they re-emit the current components instead of older payload
// values; this keeps a replayed half-group idempotent and never regresses the
// view. An empty result for a durable event means "no UI change".
func projectUIEvent(sid string, ev agent.Event, known *uiState) []a2uiMessage {
	durable := ev.DurableSeq != nil
	apply := !durable || *ev.DurableSeq > known.seedCursor
	switch ev.Type {
	case "input.accepted":
		var r agent.InputReceipt
		if json.Unmarshal(ev.Payload, &r) != nil || r.TraceID == "" {
			return nil
		}
		if known.traces[r.TraceID] == nil {
			if !apply {
				return nil
			}
			known.traces[r.TraceID] = &a2uiTask{TraceID: r.TraceID, State: "queued", TargetAgent: r.TargetAgent.Name}
			known.tasks = append(known.tasks, taskID(r.TraceID))
		}
		return []a2uiMessage{surfaceUpdate(sid, known.taskComponent(r.TraceID), known.root())}
	case "trace.state_changed", "trace.settled":
		var tr struct {
			ID      string            `json:"id"`
			State   string            `json:"state"`
			Target  agent.TargetAgent `json:"target"`
			Settled bool              `json:"settled"`
		}
		if json.Unmarshal(ev.Payload, &tr) != nil || tr.ID == "" {
			return nil
		}
		cur := known.traces[tr.ID]
		if apply {
			if cur == nil {
				cur = &a2uiTask{TraceID: tr.ID}
				known.traces[tr.ID] = cur
				known.tasks = append(known.tasks, taskID(tr.ID))
			}
			cur.State, cur.TargetAgent, cur.Settled = tr.State, tr.Target.Name, tr.Settled
		}
		if cur == nil {
			return nil
		}
		return []a2uiMessage{surfaceUpdate(sid, known.taskComponent(tr.ID), known.root())}
	case "message.finalized":
		var m agent.AgentMessage
		if json.Unmarshal(ev.Payload, &m) != nil || m.ID == "" {
			return nil
		}
		role := messageRole(m)
		if role == "" {
			return nil
		}
		if apply {
			dto := projectMessage(m)
			if known.messages[m.ID] == nil {
				known.order = append(known.order, msgID(m.ID))
			}
			known.messages[m.ID] = &uiMessage{role: role, status: "final", traceID: m.Scope.TraceID}
			known.texts[m.ID] = messageText(dto)
			for _, b := range dto.Blocks {
				if b.Type == "tool_call" {
					known.provider[m.Scope.TurnID+"\x00"+b.CallID] = m.ID
				}
			}
		}
		if known.messages[m.ID] == nil {
			return nil
		}
		return []a2uiMessage{surfaceUpdate(sid, known.messageComponent(m.ID), known.root()), dataUpdate(sid, a2uiContent{Key: textKey(m.ID), ValueString: known.texts[m.ID]})}
	case "tool.requested", "tool.state_changed", "tool.finished":
		id, name, status, owner := decodeToolEvent(ev.Payload, known)
		if id == "" {
			return nil
		}
		if apply {
			c := known.calls[id]
			if c == nil {
				if name == "" {
					return nil
				}
				c = &a2uiToolCall{CallID: id, Name: name}
				known.calls[id] = c
				known.insertCall(id, owner)
			}
			if status != "" {
				c.Status = status
			}
		}
		if known.calls[id] == nil {
			return nil
		}
		return []a2uiMessage{surfaceUpdate(sid, known.callComponent(id), known.root())}
	case "message.snapshot":
		var s agent.ModelStreamSnapshot
		if json.Unmarshal(ev.Payload, &s) != nil || s.MessageID == "" || known.traceDone(ev.Scope.TraceID) {
			return nil
		}
		cur := known.messages[s.MessageID]
		if cur != nil && cur.status == "final" {
			return nil
		}
		text := streamText(s)
		known.texts[s.MessageID] = text
		data := dataUpdate(sid, a2uiContent{Key: textKey(s.MessageID), ValueString: text})
		if cur != nil {
			return []a2uiMessage{data}
		}
		known.messages[s.MessageID] = &uiMessage{role: "assistant", status: "streaming", traceID: ev.Scope.TraceID}
		known.order = append(known.order, msgID(s.MessageID))
		return []a2uiMessage{surfaceUpdate(sid, known.messageComponent(s.MessageID), known.root()), data}
	case "tool.output.delta":
		var d agent.ToolOutputDelta
		if json.Unmarshal(ev.Payload, &d) != nil || known.calls[d.ToolCallID] == nil || known.traceDone(ev.Scope.TraceID) {
			return nil
		}
		known.outputs[d.ToolCallID] = boundPreview(known.outputs[d.ToolCallID] + d.Text)
		return []a2uiMessage{dataUpdate(sid, a2uiContent{Key: outputKey(d.ToolCallID), ValueString: known.outputs[d.ToolCallID]})}
	}
	return nil
}

// decodeToolEvent reads either a ToolRecord payload or the reconciliation
// summary {callId,status}. Only identity, name and status leave this function.
func decodeToolEvent(raw json.RawMessage, known *uiState) (id, name, status, owner string) {
	var rec agent.ToolRecord
	if json.Unmarshal(raw, &rec) == nil && rec.Call.CallID != "" {
		return rec.Call.CallID, rec.Call.Name, toolStatus(rec), known.provider[rec.Scope.TurnID+"\x00"+rec.Call.ProviderCallID]
	}
	var sum struct {
		CallID string `json:"callId"`
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &sum) == nil && sum.CallID != "" {
		return sum.CallID, "", sum.Status, ""
	}
	return "", "", "", ""
}
