package web

import (
	"bufio"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/testkit"
)

// a2uiStore applies frames the way the browser store does: components and
// data keys overwrite by ID, so replaying a frame is idempotent.
type a2uiStore struct {
	root       string
	components map[string]string
	data       map[string]string
}

func newA2UIStore() *a2uiStore {
	return &a2uiStore{components: map[string]string{}, data: map[string]string{}}
}

func (s *a2uiStore) apply(t *testing.T, raw string) {
	t.Helper()
	var generic map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &generic); err != nil || len(generic) != 1 {
		t.Fatalf("message must have exactly one field: %s", raw)
	}
	var msg a2uiMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	switch {
	case msg.BeginRendering != nil:
		s.root = msg.BeginRendering.Root
	case msg.SurfaceUpdate != nil:
		for _, c := range msg.SurfaceUpdate.Components {
			var fields map[string]json.RawMessage
			b, _ := json.Marshal(c.Component)
			if json.Unmarshal(b, &fields) != nil || len(fields) != 1 {
				t.Fatalf("component must have exactly one kind: %s", b)
			}
			s.components[c.ID] = string(b)
		}
	case msg.DataModelUpdate != nil:
		for _, c := range msg.DataModelUpdate.Contents {
			s.data[c.Key] = c.ValueString
		}
	}
}

func encodeAll(t *testing.T, msgs []a2uiMessage) string {
	t.Helper()
	var b strings.Builder
	for _, m := range msgs {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

func renderLines(t *testing.T, s *Server, token, sid string) ([]string, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", s.URL()+"/v1/sessions/"+sid+"/render", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal("render request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/jsonl") {
		t.Fatalf("render status=%d type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, resp.Header.Get("X-Session-Cursor")
}

func userMessage(id, trace, text string) agent.AgentMessage {
	return agent.AgentMessage{ID: id, Kind: agent.KindUser, Status: agent.StatusComplete, Scope: agent.MessageScope{TraceID: trace}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.UserInputText{Text: text})}}}
}

func assistantMessage(id, trace, text string) agent.AgentMessage {
	return agent.AgentMessage{ID: id, Kind: agent.KindAssistant, Status: agent.StatusComplete, Scope: agent.MessageScope{TraceID: trace}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, Extra: map[string]any{"vendor": "provider-extra-value"}, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: text})}}}
}

func durableEvent(t *testing.T, typ, trace string, seq uint64, payload any) agent.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return agent.Event{SchemaVersion: 1, Type: typ, Scope: agent.EventScope{SessionID: "s1", TraceID: trace}, DurableSeq: &seq, Payload: raw}
}

func tempEvent(t *testing.T, typ, trace string, payload any) agent.Event {
	t.Helper()
	ev := durableEvent(t, typ, trace, 0, payload)
	ev.DurableSeq = nil
	return ev
}

func emptySnapshot(cursor uint64) sessions.Snapshot {
	return sessions.Snapshot{SessionID: "s1", Cursor: cursor, Traces: map[string]*state.TraceState{}, Inputs: map[string]*state.InputState{}}
}

func TestA2UIRenderGoldenStructureForCompletedPrompt(t *testing.T) {
	s, token, c, _ := offlineServer(t, testkit.Step{Text: "answer <b>bold</b>"})
	sid := createSession(t, s, token, c)
	_, receipt := call(t, s, token, "POST", "/v1/sessions/"+sid+"/inputs", "i", map[string]any{"kind": "prompt", "content": []map[string]string{{"type": "text", "text": "hi"}}})
	tid, _ := receipt["traceId"].(string)
	snap := pollSnapshot(t, s, token, sid, func(m map[string]any) bool { return traceState(m, tid) == "completed" })
	lines, cursor := renderLines(t, s, token, sid)
	if cursor != snap["cursor"] {
		t.Fatalf("X-Session-Cursor=%q snapshot cursor=%v", cursor, snap["cursor"])
	}
	if len(lines) < 2 || !strings.HasPrefix(lines[0], `{"beginRendering":{"surfaceId":"session:`+sid+`","root":"root"}`) {
		t.Fatalf("first line is not beginRendering: %v", lines)
	}
	store := newA2UIStore()
	for _, l := range lines {
		store.apply(t, l)
	}
	var root a2uiComponentValue
	_ = json.Unmarshal([]byte(store.components["root"]), &root)
	if root.Column == nil || len(root.Column.Children) != 3 {
		t.Fatalf("root children=%v", store.components["root"])
	}
	var roles []string
	for _, child := range root.Column.Children[:2] {
		var v a2uiComponentValue
		_ = json.Unmarshal([]byte(store.components[child]), &v)
		m := v.ChatMessage
		if m == nil || child != "msg:"+m.MessageID || m.DataKey != "text:"+m.MessageID || m.Status != "final" {
			t.Fatalf("message component %s=%s", child, store.components[child])
		}
		roles = append(roles, m.Role+"="+store.data[m.DataKey])
	}
	if strings.Join(roles, "|") != "user=hi|assistant=answer <b>bold</b>" {
		t.Fatalf("messages=%v", roles)
	}
	var task a2uiComponentValue
	_ = json.Unmarshal([]byte(store.components["task:"+tid]), &task)
	if root.Column.Children[2] != "task:"+tid || task.Task == nil || task.Task.State != "completed" || task.Task.TargetAgent == "" {
		t.Fatalf("task=%s", store.components["task:"+tid])
	}
	again, _ := renderLines(t, s, token, sid)
	if !reflect.DeepEqual(lines, again) {
		t.Fatal("two renders of the same state differ")
	}
	joined := strings.ToLower(strings.Join(lines, "\n"))
	for _, private := range []string{"principal", "generation", "grant", "provider-extra"} {
		if strings.Contains(joined, private) {
			t.Fatalf("render leaked %q", private)
		}
	}
}

func TestA2UIEventsReplayIsIdempotentAndMatchesRender(t *testing.T) {
	s, token, c, _ := offlineServer(t, testkit.Step{Text: "answer"})
	sid := createSession(t, s, token, c)
	_, receipt := call(t, s, token, "POST", "/v1/sessions/"+sid+"/inputs", "i", map[string]any{"kind": "prompt", "content": []map[string]string{{"type": "text", "text": "hi"}}})
	tid, _ := receipt["traceId"].(string)
	snap := pollSnapshot(t, s, token, sid, func(m map[string]any) bool { return traceState(m, tid) == "completed" })
	last, _ := snap["cursor"].(string)
	untilLast := func(f []sseFrame) bool { return f[len(f)-1].id == last }
	product, _ := readFrames(t, s, token, "/v1/sessions/"+sid+"/events", "", untilLast)
	all, status := readFrames(t, s, token, "/v1/sessions/"+sid+"/ui/events", "", untilLast)
	if status != 200 || all[0].event != "ready" {
		t.Fatalf("ui stream status=%d first=%+v", status, all)
	}
	var productIDs, ids []string
	for _, f := range product {
		if f.id != "" {
			productIDs = append(productIDs, f.id)
		}
	}
	full := newA2UIStore()
	for _, f := range all[1:] {
		if f.id != "" {
			ids = append(ids, f.id)
		}
		switch f.event {
		case "a2ui":
			full.apply(t, f.data)
		case "cursor":
			if f.id == "" || f.data != "{}" {
				t.Fatalf("cursor frame without id: %+v", f)
			}
		default:
			t.Fatalf("unexpected frame %+v", f)
		}
	}
	// Every durable fact advances the UI cursor exactly once, in order.
	if len(ids) < 3 || strings.Join(ids, ",") != strings.Join(productIDs, ",") {
		t.Fatalf("ui ids %v\nproduct ids %v", ids, productIDs)
	}
	lines, _ := renderLines(t, s, token, sid)
	rendered := newA2UIStore()
	for _, l := range lines {
		rendered.apply(t, l)
	}
	if !reflect.DeepEqual(full.components, rendered.components) || !reflect.DeepEqual(full.data, rendered.data) {
		t.Fatalf("replayed surface differs from render\n%v\n%v", full.components, rendered.components)
	}
	// Resume from a mid cursor: the suffix only, and reapplying a half group
	// already seen leaves the surface unchanged.
	mid := ids[1]
	resumed, _ := readFrames(t, s, token, "/v1/sessions/"+sid+"/ui/events", mid, untilLast)
	partial := newA2UIStore()
	for _, f := range all[1:] {
		if f.event == "a2ui" {
			partial.apply(t, f.data)
		}
		if f.id == mid {
			break
		}
	}
	var got []string
	for _, f := range resumed[1:] {
		if f.id != "" {
			got = append(got, f.id)
		}
		if f.event == "a2ui" {
			partial.apply(t, f.data)
		}
	}
	if strings.Join(got, ",") != strings.Join(ids[2:], ",") {
		t.Fatalf("resume ids %v want %v", got, ids[2:])
	}
	if !reflect.DeepEqual(partial.components, full.components) || !reflect.DeepEqual(partial.data, full.data) {
		t.Fatal("resumed surface is not idempotent with the full replay")
	}
	for _, bad := range []string{"?cursor=garbage", "?cursor=" + EncodeCursor("other", 1), "?cursor=" + EncodeCursor(sid, 1<<40)} {
		if _, status = readFrames(t, s, token, "/v1/sessions/"+sid+"/ui/events"+bad, "", func([]sseFrame) bool { return true }); status == 200 {
			t.Fatalf("bad cursor %s accepted", bad)
		}
	}
	if _, status = readFrames(t, s, token, "/v1/sessions/"+sid+"/ui/events?cursor="+ids[0], ids[1], func([]sseFrame) bool { return true }); status != 400 {
		t.Fatalf("conflicting Last-Event-ID status=%d", status)
	}
	for _, f := range all {
		low := strings.ToLower(f.data)
		if strings.Contains(low, "principal") || strings.Contains(low, "generation") || strings.Contains(low, "grant") {
			t.Fatalf("private field in ui frame: %s", f.data)
		}
	}
}

func TestA2UIStreamingSnapshotThenFinalUpdatesSameKey(t *testing.T) {
	known := newUIState("s1", emptySnapshot(5), "inst")
	snapshot := agent.ModelStreamSnapshot{AttemptID: "a1", MessageID: "m1", StreamID: "st", ChunkSeq: 1, Blocks: []agent.ModelStreamBlock{{Type: "text", Text: "par"}, {Type: "reasoning", Text: "hidden-reasoning"}}}
	first := projectUIEvent("s1", tempEvent(t, "message.snapshot", "t1", snapshot), known)
	if len(first) != 2 || first[0].SurfaceUpdate == nil || first[1].DataModelUpdate == nil {
		t.Fatalf("first snapshot frames=%s", encodeAll(t, first))
	}
	comp := first[0].SurfaceUpdate.Components[0]
	if comp.ID != "msg:m1" || comp.Component.ChatMessage.Status != "streaming" || comp.Component.ChatMessage.DataKey != "text:m1" {
		t.Fatalf("streaming component=%+v", comp)
	}
	if strings.Contains(encodeAll(t, first), "hidden-reasoning") {
		t.Fatal("reasoning leaked into A2UI")
	}
	snapshot.ChunkSeq, snapshot.Blocks = 2, []agent.ModelStreamBlock{{Type: "text", Text: "partial"}}
	second := projectUIEvent("s1", tempEvent(t, "message.snapshot", "t1", snapshot), known)
	if len(second) != 1 || second[0].DataModelUpdate.Contents[0] != (a2uiContent{Key: "text:m1", ValueString: "partial"}) {
		t.Fatalf("second snapshot must only replace the bound text: %s", encodeAll(t, second))
	}
	final := projectUIEvent("s1", durableEvent(t, "message.finalized", "t1", 6, assistantMessage("m1", "t1", "partial done")), known)
	if len(final) != 2 || final[0].SurfaceUpdate.Components[0].ID != "msg:m1" || final[0].SurfaceUpdate.Components[0].Component.ChatMessage.Status != "final" || final[1].DataModelUpdate.Contents[0] != (a2uiContent{Key: "text:m1", ValueString: "partial done"}) {
		t.Fatalf("final frames=%s", encodeAll(t, final))
	}
	root := final[0].SurfaceUpdate.Components[1]
	if root.ID != "root" || !reflect.DeepEqual(root.Component.Column.Children, []string{"msg:m1"}) {
		t.Fatalf("final duplicated the message in root: %+v", root)
	}
	if late := projectUIEvent("s1", tempEvent(t, "message.snapshot", "t1", snapshot), known); late != nil {
		t.Fatalf("late snapshot overwrote a final message: %s", encodeAll(t, late))
	}
}

func TestA2UITerminalTaskIsNotRevivedByLateTemporaryEvents(t *testing.T) {
	snap := emptySnapshot(3)
	snap.Traces["t1"] = &state.TraceState{ID: "t1", State: "running"}
	snap.Calls = map[string]agent.ToolRecord{"c1": {Call: agent.FrozenCall{CallID: "c1", ProviderCallID: "p1", Name: "read"}, Claimed: true}}
	known := newUIState("s1", snap, "inst")
	known.calls["c1"] = &a2uiToolCall{CallID: "c1", Name: "read", Status: "running"}
	known.insertCall("c1", "")
	if got := projectUIEvent("s1", tempEvent(t, "tool.output.delta", "t1", agent.ToolOutputDelta{ToolCallID: "c1", Stream: "stdout", Text: "out"}), known); len(got) != 1 {
		t.Fatalf("live tool output frames=%s", encodeAll(t, got))
	}
	done := projectUIEvent("s1", durableEvent(t, "trace.state_changed", "t1", 4, state.TraceState{ID: "t1", State: "completed", Settled: true}), known)
	if len(done) != 1 || done[0].SurfaceUpdate.Components[0].Component.Task.State != "completed" {
		t.Fatalf("terminal frames=%s", encodeAll(t, done))
	}
	late := []agent.Event{
		tempEvent(t, "tool.output.delta", "t1", agent.ToolOutputDelta{ToolCallID: "c1", Stream: "stdout", Text: "late"}),
		tempEvent(t, "message.snapshot", "t1", agent.ModelStreamSnapshot{MessageID: "m9", StreamID: "s", ChunkSeq: 1, Blocks: []agent.ModelStreamBlock{{Type: "text", Text: "ghost"}}}),
	}
	for _, ev := range late {
		if got := projectUIEvent("s1", ev, known); got != nil {
			t.Fatalf("late %s revived output: %s", ev.Type, encodeAll(t, got))
		}
	}
	if known.messages["m9"] != nil || known.outputs["c1"] != "out" {
		t.Fatal("late temporary event changed known state")
	}
	// A durable fact at or before the seed cursor re-emits the current view
	// rather than regressing to its older payload.
	old := projectUIEvent("s1", durableEvent(t, "trace.state_changed", "t1", 2, state.TraceState{ID: "t1", State: "running"}), newUIState("s1", func() sessions.Snapshot {
		s := emptySnapshot(4)
		s.Traces["t1"] = &state.TraceState{ID: "t1", State: "completed", Settled: true}
		return s
	}(), ""))
	if len(old) != 1 || old[0].SurfaceUpdate.Components[0].Component.Task.State != "completed" {
		t.Fatalf("replayed old fact regressed the task: %s", encodeAll(t, old))
	}
}

func TestA2UIApprovalMapsInteractionAndLeaksNothingPrivate(t *testing.T) {
	snap := emptySnapshot(9)
	snap.Traces["t1"] = &state.TraceState{ID: "t1", State: "paused", Generation: "gen-value-x", Target: agent.TargetAgent{Name: "main", Generation: "gen-value-x"}}
	snap.Inputs["in1"] = &state.InputState{ID: "in1", TraceID: "t1", Principal: "local-principal-value", CommitSeq: 1}
	snap.Messages = []agent.AgentMessage{userMessage("u1", "t1", "<script>alert(1)</script>"), assistantMessage("a1", "t1", "ok")}
	snap.Interactions = map[string]state.Interaction{
		"i1":      {ID: "i1", Kind: "approval", State: "ready", Question: "Approve this operation once?", Options: []string{"allowed-once", "rejected", "cancelled"}, Scope: agent.ExecutionScope{TraceID: "t1", Generation: "gen-value-x"}, TargetRef: "eino-interrupt-address-value", CheckpointRef: "checkpoint-ref-value", ApprovalID: "approval-private-id"},
		"pending": {ID: "pending", Kind: "approval", State: "pending", Question: "not safely stopped"},
		"expired": {ID: "expired", Kind: "approval", State: "expired", Question: "old"},
		"ask":     {ID: "ask", Kind: "question", State: "pending", Question: "not an approval"},
	}
	snap.Approvals = map[string]state.Approval{"approval-private-id": {ID: "approval-private-id", GrantRef: "grant-ref-value", Principal: "local-principal-value", FrozenHash: "frozen-hash-value"}}
	out := encodeAll(t, renderSnapshot("s1", snap, "instance-1"))
	store := newA2UIStore()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		store.apply(t, line)
	}
	var v a2uiComponentValue
	_ = json.Unmarshal([]byte(store.components["approval:i1"]), &v)
	if v.Approval == nil || v.Approval.InteractionID != "i1" || v.Approval.TraceID != "t1" || v.Approval.InstanceID != "instance-1" || len(v.Approval.Options) != 3 {
		t.Fatalf("approval=%s", store.components["approval:i1"])
	}
	if _, ok := store.components["approval:pending"]; ok {
		t.Fatal("approval rendered before the execution safely stopped")
	}
	if _, ok := store.components["approval:expired"]; ok {
		t.Fatal("non-pending approval rendered")
	}
	if _, ok := store.components["approval:ask"]; ok {
		t.Fatal("non-approval interaction rendered")
	}
	if store.data["text:u1"] != "<script>alert(1)</script>" {
		t.Fatal("user text must be sent verbatim as plain data")
	}
	low := strings.ToLower(out)
	for _, private := range []string{"principal", "generation", "grant", "gen-value-x", "eino-interrupt", "checkpoint-ref", "approval-private-id", "frozen-hash", "provider-extra"} {
		if strings.Contains(low, private) {
			t.Fatalf("render leaked %q:\n%s", private, out)
		}
	}
	// Without a runtime instance (read-only or reopened) no approval is shown,
	// and an instance change removes the component from root.
	if strings.Contains(encodeAll(t, renderSnapshot("s1", snap, "")), "approval:") {
		t.Fatal("approval rendered without a runtime instance")
	}
	known := newUIState("s1", snap, "instance-1")
	delete(snap.Interactions, "i1")
	if !known.syncApprovals(snap, "instance-1") || strings.Contains(encodeAll(t, known.approvalFrames()), "approval:i1") {
		t.Fatal("resolved approval still visible")
	}
}

// Progress components carry identity, name and state only: child results,
// node results and error text never reach the browser.
func TestProgressComponentsExposeOnlyIdentityAndState(t *testing.T) {
	inv := invocationComponent(state.Invocation{ID: "inv-1", ParentInvocationID: "root", ParentCallID: "call-1", TraceID: "tr", Target: agent.TargetAgent{Name: "reviewer"}, State: "running", Result: "SECRET-RESULT", ModelCalls: 3})
	node := workflowNodeComponent(state.WorkflowNodeRun{ID: "inv-1:t:1", TraceID: "tr", InvocationID: "inv-1", NodeID: "t", Kind: "tool", State: "failed", Result: "SECRET-RESULT", Error: "SECRET-ERROR", ToolCallID: "inv-1:t:1"})
	if inv.ID != "inv:inv-1" || node.ID != "node:inv-1:t:1" {
		t.Fatalf("ids %q %q", inv.ID, node.ID)
	}
	for _, c := range []a2uiComponent{inv, node} {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "modelCalls") {
			t.Fatalf("private field leaked: %s", b)
		}
		var fields map[string]json.RawMessage
		cb, _ := json.Marshal(c.Component)
		if json.Unmarshal(cb, &fields) != nil || len(fields) != 1 {
			t.Fatalf("component must have exactly one kind: %s", cb)
		}
	}
	want := `{"id":"inv:inv-1","component":{"Invocation":{"invocationId":"inv-1","parentCallId":"call-1","agent":"reviewer","state":"running"}}}`
	if b, _ := json.Marshal(inv); string(b) != want {
		t.Fatalf("invocation %s", b)
	}
	want = `{"id":"node:inv-1:t:1","component":{"WorkflowNode":{"nodeExecutionId":"inv-1:t:1","traceId":"tr","nodeId":"t","kind":"tool","state":"failed"}}}`
	if b, _ := json.Marshal(node); string(b) != want {
		t.Fatalf("node %s", b)
	}
}

// Render places Invocation and WorkflowNode after tasks and before approvals,
// each sorted by product ID; a later snapshot refresh reports changed state.
func TestA2UIRenderAndRefreshProgressComponents(t *testing.T) {
	snap := emptySnapshot(4)
	snap.Traces["t1"] = &state.TraceState{ID: "t1", State: "running", Target: agent.TargetAgent{Name: "main"}}
	snap.Inputs["in1"] = &state.InputState{ID: "in1", TraceID: "t1", CommitSeq: 1}
	snap.Interactions = map[string]state.Interaction{"i1": {ID: "i1", Kind: "approval", State: "ready", Scope: agent.ExecutionScope{TraceID: "t1"}}}
	snap.Invocations = map[string]state.Invocation{
		"b": {ID: "b", TraceID: "t1", Target: agent.TargetAgent{Name: "writer"}, State: "running"},
		"a": {ID: "a", TraceID: "t1", Target: agent.TargetAgent{Name: "reviewer"}, State: "completed"},
	}
	snap.WorkflowNodes = map[string]state.WorkflowNodeRun{"w:n:1": {ID: "w:n:1", TraceID: "t1", NodeID: "n", Kind: "tool", State: "running"}}
	store := newA2UIStore()
	for _, line := range strings.Split(strings.TrimSpace(encodeAll(t, renderSnapshot("s1", snap, "inst"))), "\n") {
		store.apply(t, line)
	}
	var root a2uiComponentValue
	_ = json.Unmarshal([]byte(store.components[a2uiRootID]), &root)
	want := []string{"task:t1", "inv:a", "inv:b", "node:w:n:1", "approval:i1"}
	if root.Column == nil || !slices.Equal(root.Column.Children, want) {
		t.Fatalf("root children %s", store.components[a2uiRootID])
	}
	var v a2uiComponentValue
	_ = json.Unmarshal([]byte(store.components["inv:b"]), &v)
	if v.Invocation == nil || v.Invocation.State != "running" || v.Invocation.Agent != "writer" {
		t.Fatalf("invocation %s", store.components["inv:b"])
	}

	known := newUIState("s1", snap, "inst")
	if known.syncProgress(snap) {
		t.Fatal("unchanged progress reported as changed")
	}
	inv := snap.Invocations["b"]
	inv.State = "completed"
	snap.Invocations["b"] = inv
	if !known.syncProgress(snap) {
		t.Fatal("changed invocation state not reported")
	}
	frames := encodeAll(t, known.progressFrames())
	if !strings.Contains(frames, `"invocationId":"b","parentCallId":"","agent":"writer","state":"completed"`) || !strings.Contains(frames, `"node:w:n:1"`) {
		t.Fatalf("progress frames %s", frames)
	}
}

// The Eino adapter publishes text blocks with the schema type name; the
// projection must show them while still dropping reasoning.
func TestA2UIStreamingSnapshotAcceptsEinoTextBlockType(t *testing.T) {
	known := newUIState("s1", emptySnapshot(1), "inst")
	snap := agent.ModelStreamSnapshot{AttemptID: "a1", MessageID: "m1", StreamID: "st", ChunkSeq: 1, Blocks: []agent.ModelStreamBlock{{Type: string(schema.ContentBlockTypeReasoning), Text: "hidden-reasoning"}, {Type: string(schema.ContentBlockTypeAssistantGenText), Text: "你好😀"}}}
	frames := projectUIEvent("s1", tempEvent(t, "message.snapshot", "t1", snap), known)
	if len(frames) != 2 || frames[1].DataModelUpdate == nil || frames[1].DataModelUpdate.Contents[0] != (a2uiContent{Key: "text:m1", ValueString: "你好😀"}) {
		t.Fatalf("frames=%s", encodeAll(t, frames))
	}
	if strings.Contains(encodeAll(t, frames), "hidden-reasoning") {
		t.Fatal("reasoning leaked into A2UI")
	}
}
