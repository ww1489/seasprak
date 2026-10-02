package workflowagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

func (w *WorkflowAgent) eventScope(node string) agent.EventScope {
	return agent.EventScope{WorkflowRunID: w.opts.RunID, NodeExecutionID: node}
}
func (w *WorkflowAgent) event(kind, node string, v any) agent.Event {
	raw, _ := json.Marshal(v)
	return agent.Event{SchemaVersion: 1, Type: kind, Scope: w.eventScope(node), EventID: agent.MustID(), OccurredAt: time.Now().UTC(), Payload: raw}
}
func (w *WorkflowAgent) publishLocked(ev agent.Event) {
	for sub := range w.subs {
		if !sub.enqueue(ev) {
			delete(w.subs, sub)
		}
	}
}
func encodeCursor(id string, seq uint64) string {
	raw, _ := json.Marshal([]any{2, "workflow", id, strconv.FormatUint(seq, 10)})
	return base64.RawURLEncoding.EncodeToString(raw)
}
func decodeCursor(cursor, id string) (uint64, error) {
	bad := func() (uint64, error) {
		return 0, product.NewError(product.CodeInvalidArgument, "invalid workflow cursor")
	}
	if cursor == "" {
		return 0, nil
	}
	if len(cursor) > 512 {
		return bad()
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != cursor {
		return bad()
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) != 4 || string(parts[0]) != "2" {
		return bad()
	}
	var kind, run, number string
	if json.Unmarshal(parts[1], &kind) != nil || json.Unmarshal(parts[2], &run) != nil || json.Unmarshal(parts[3], &number) != nil || kind != "workflow" || run != id {
		return bad()
	}
	seq, err := strconv.ParseUint(number, 10, 64)
	if err != nil || encodeCursor(id, seq) != cursor {
		return bad()
	}
	return seq, nil
}

func (w *WorkflowAgent) Snapshot(ctx context.Context) (WorkflowSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowSnapshot{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return WorkflowSnapshot{}, product.NewError(product.CodeStateConflict, "workflow is closed")
	}
	s := w.state
	v := WorkflowSnapshot{ModelAttempts: map[string]WorkflowModelAttemptView{}, Observations: map[string]WorkflowObservationView{}, RunID: w.opts.RunID, DefinitionName: s.Initial.Manifest.Definition.Name, DefinitionVersion: s.Initial.Manifest.Definition.Version, BindingVersion: s.Initial.BindingVersion, State: s.Run.State, Revision: s.Revision, DurableSeq: s.Cursor, Cursor: encodeCursor(w.opts.RunID, s.Cursor), EarliestReplayCursor: encodeCursor(w.opts.RunID, 0), InstanceID: w.instance, Workspace: s.Initial.Workspace, ExecutionStopped: s.Run.ExecutionStopped, WorkflowNodes: copyMap(s.Nodes), Interactions: map[string]WorkflowInteraction{}, Operations: map[string]WorkflowOperation{}, Usage: s.Usage, Result: append(json.RawMessage(nil), s.Run.Result...), ErrorCode: s.Run.ErrorCode, FailedNode: s.Run.FailedNode, RepairRequired: s.RepairRequired, Transient: WorkflowTransient{Models: map[string]agent.ModelStreamSnapshot{}, Tools: copyMap(w.transient.Tools)}}
	for id, a := range s.Attempts {
		status := a.Status
		if status == "complete" {
			status = "accepted"
		}
		view := WorkflowModelAttemptView{ID: id, NodeExecutionID: a.Scope.NodeExecutionID, ModelCallID: a.Identity.ModelCallID, Status: status}
		if a.Result != nil {
			view.FailureCode = a.Result.Details.FailureCode
		}
		v.ModelAttempts[id] = view
	}
	for id, call := range s.Calls {
		if o := call.Observation; o != nil {
			v.Observations[id] = WorkflowObservationView{ToolCallID: id, NodeExecutionID: call.Scope.NodeExecutionID, Status: o.Status, SideEffect: o.SideEffect, Executed: o.Executed, Process: o.Process, Terminated: o.Terminated, ExitCode: o.ExitCode}
		}
	}
	for id, p := range w.approvals {
		view := p.view
		view.Options = append([]string(nil), view.Options...)
		v.Interactions[id] = view
	}
	for id, op := range s.Operations {
		v.Operations[id] = WorkflowOperation{Receipt: op.Receipt, Kind: op.Kind, State: op.State}
	}
	for id, op := range w.approvalOps {
		v.Operations[id] = op
	}
	for id, m := range w.transient.Models {
		m.Blocks = append([]agent.ModelStreamBlock(nil), m.Blocks...)
		v.Transient.Models[id] = m
	}
	if err := w.resumeErrorLocked(); err == nil && !w.opts.ReadOnly {
		v.CanResume = true
	} else if err != nil {
		v.ResumeCode = errorCode(err)
	}
	return v, nil
}

type queuedEvent struct {
	event agent.Event
	size  int
}
type WorkflowSubscription struct {
	Events   <-chan agent.Event
	Handoff  uint64
	owner    *WorkflowAgent
	mu       sync.Mutex
	queue    []queuedEvent
	handoff  []queuedEvent
	bytes    int
	limits   config.Limits
	done     chan struct{}
	wake     chan struct{}
	stopped  chan struct{}
	finished bool
	err      error
}

func cloneEvent(ev agent.Event) agent.Event {
	ev.Payload = append(json.RawMessage(nil), ev.Payload...)
	if ev.DurableSeq != nil {
		seq := *ev.DurableSeq
		ev.DurableSeq = &seq
	}
	if ev.ChunkSeq != nil {
		seq := *ev.ChunkSeq
		ev.ChunkSeq = &seq
	}
	return ev
}
func (s *WorkflowSubscription) enqueue(ev agent.Event) bool {
	raw, _ := json.Marshal(ev)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return false
	}
	if len(s.queue)+len(s.handoff) >= s.limits.SubscriptionEvents || s.bytes+len(raw) > s.limits.SubscriptionBytes {
		s.finishLocked(product.NewError(product.CodeResyncRequired, "workflow subscriber fell behind"))
		return false
	}
	s.queue = append(s.queue, queuedEvent{event: cloneEvent(ev), size: len(raw)})
	s.bytes += len(raw)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return true
}
func (s *WorkflowSubscription) finishLocked(err error) {
	if !s.finished {
		s.finished = true
		s.err = err
		close(s.done)
	}
}
func (s *WorkflowSubscription) finish(err error) { s.mu.Lock(); s.finishLocked(err); s.mu.Unlock() }
func (s *WorkflowSubscription) Close() {
	s.finish(nil)
	<-s.stopped
	s.owner.mu.Lock()
	delete(s.owner.subs, s)
	s.owner.mu.Unlock()
}
func (s *WorkflowSubscription) Err() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (w *WorkflowAgent) SubscribeFrom(ctx context.Context, opts WorkflowSubscribeOptions) (*WorkflowSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	after := opts.After
	if opts.Cursor != "" {
		cursor, err := decodeCursor(opts.Cursor, w.opts.RunID)
		if err != nil {
			return nil, err
		}
		if after != 0 && after != cursor {
			return nil, product.NewError(product.CodeInvalidArgument, "conflicting workflow cursors")
		}
		after = cursor
	}
	w.mu.Lock()
	if w.closed || w.closing {
		w.mu.Unlock()
		return nil, product.NewError(product.CodeStateConflict, "workflow is closing")
	}
	if after > w.state.Cursor {
		w.mu.Unlock()
		return nil, product.NewError(product.CodeInvalidArgument, "workflow cursor is in the future")
	}
	out := make(chan agent.Event)
	s := &WorkflowSubscription{Events: out, Handoff: w.state.Cursor, owner: w, limits: opts.Limits.WithDefaults(), done: make(chan struct{}), wake: make(chan struct{}, 1), stopped: make(chan struct{})}
	for _, ev := range w.transientHandoffLocked() {
		raw, _ := json.Marshal(ev)
		if s.bytes+len(raw) > s.limits.SubscriptionBytes || len(s.handoff) >= s.limits.SubscriptionEvents {
			s.finish(product.NewError(product.CodeResyncRequired, "workflow handoff exceeds subscriber limits"))
			break
		}
		s.handoff = append(s.handoff, queuedEvent{event: ev, size: len(raw)})
		s.bytes += len(raw)
	}
	w.subs[s] = struct{}{}
	historyOnly := w.opts.ReadOnly
	w.mu.Unlock()
	go s.deliver(ctx, after, historyOnly, out)
	return s, nil
}
func (w *WorkflowAgent) transientHandoffLocked() []agent.Event {
	var events []agent.Event
	add := func(kind, node, stream string, seq uint64, view any) {
		// Encoding under the coordinator lock owns every referenced slice and
		// string at B. These are replacement views, not new deltas or permission.
		payload, _ := json.Marshal(view)
		events = append(events, agent.Event{SchemaVersion: 1, Type: kind, Scope: w.eventScope(node), StreamID: stream, ChunkSeq: &seq, OccurredAt: time.Now().UTC(), Payload: payload})
	}
	for stream, snapshot := range w.transient.Models {
		attempt := w.state.Attempts[snapshot.AttemptID]
		add("message.snapshot", attempt.Scope.NodeExecutionID, stream, snapshot.ChunkSeq, snapshot)
	}
	for _, preview := range w.transient.Tools {
		stream := w.callStreams[preview.ToolCallID]
		add("tool.output.snapshot", w.state.Calls[preview.ToolCallID].Scope.NodeExecutionID, stream, w.streamSeq[stream], preview)
	}
	for id, pending := range w.approvals {
		if pending.decision == "" && (pending.view.State == "pending" || pending.view.State == "ready") && !terminal(w.state.Run.State) {
			add("interaction.requested", pending.view.NodeExecutionID, w.instance+":"+id, 1, pending.view)
		}
	}
	return events
}

func (s *WorkflowSubscription) send(ctx context.Context, out chan<- agent.Event, ev agent.Event) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	select {
	case out <- ev:
		return true
	case <-s.done:
		return false
	case <-ctx.Done():
		s.finish(nil)
		return false
	}
}
func (s *WorkflowSubscription) deliver(ctx context.Context, after uint64, historyOnly bool, out chan<- agent.Event) {
	defer func() {
		s.finish(nil)
		s.mu.Lock()
		s.queue, s.handoff, s.bytes = nil, nil, 0
		s.mu.Unlock()
		s.owner.mu.Lock()
		delete(s.owner.subs, s)
		s.owner.mu.Unlock()
		close(out)
		close(s.stopped)
	}()
	for after < s.Handoff {
		s.owner.mu.Lock()
		page := make([]agent.Event, 0, 256)
		for _, ev := range s.owner.state.Events {
			if ev.DurableSeq != nil && *ev.DurableSeq > after && *ev.DurableSeq <= s.Handoff {
				page = append(page, cloneEvent(ev))
				if len(page) == 256 {
					break
				}
			}
		}
		s.owner.mu.Unlock()
		if len(page) == 0 {
			return
		}
		for _, ev := range page {
			seq := *ev.DurableSeq
			if !s.send(ctx, out, ev) {
				return
			}
			after = seq
		}
	}
	s.mu.Lock()
	handoff := append([]queuedEvent(nil), s.handoff...)
	s.mu.Unlock()
	for _, item := range handoff {
		if !s.send(ctx, out, item.event) {
			return
		}
		s.mu.Lock()
		s.handoff = s.handoff[1:]
		s.bytes -= item.size
		s.mu.Unlock()
	}
	if historyOnly {
		s.finish(nil)
		return
	}
	for {
		s.mu.Lock()
		if s.finished {
			s.queue = nil
			s.bytes = 0
			s.mu.Unlock()
			return
		}
		if len(s.queue) == 0 {
			s.mu.Unlock()
			select {
			case <-s.wake:
				continue
			case <-s.done:
				return
			case <-ctx.Done():
				s.finish(nil)
				return
			}
		}
		item := s.queue[0]
		s.mu.Unlock()
		if !s.send(ctx, out, item.event) {
			return
		}
		s.mu.Lock()
		s.queue[0] = queuedEvent{}
		s.queue = s.queue[1:]
		s.bytes -= item.size
		s.mu.Unlock()
	}
}
func (w *WorkflowAgent) modelSnapshotLocked(ctx context.Context, scope agent.ExecutionScope, raw json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.active.ctx.Err() != nil || w.state.Run.ExecutionStopped {
		return product.NewError(product.CodeStateConflict, "model stream has stopped")
	}
	var update agent.ModelStreamSnapshot
	if err := decode(raw, &update); err != nil {
		return err
	}
	a, ok := w.state.Attempts[update.AttemptID]
	if !ok || a.Status != "started" || a.Scope != scope || a.Identity.MessageID != update.MessageID || a.Identity.StreamID != update.StreamID || update.ChunkSeq <= w.streamSeq[update.StreamID] || update.ChunkSeq == 0 {
		return product.NewError(product.CodeStateConflict, "model stream snapshot is stale")
	}
	textBytes := 0
	for i, b := range update.Blocks {
		textBytes += len(b.Text) + len(b.Arguments)
		if textBytes > config.TransientToolPreviewBytes {
			remaining := max(0, config.TransientToolPreviewBytes-(textBytes-len(b.Text)-len(b.Arguments)))
			b.Text = tailText(b.Text, remaining)
			b.Arguments = ""
			update.Blocks = update.Blocks[:i+1]
			update.Blocks[i] = b
			break
		}
	}
	w.streamSeq[update.StreamID] = update.ChunkSeq
	w.transient.Models[update.StreamID] = update
	w.boundTransientLocked()
	payload, _ := json.Marshal(update)
	w.publishLocked(agent.Event{SchemaVersion: 1, Type: "message.snapshot", Scope: w.eventScope(scope.NodeExecutionID), StreamID: update.StreamID, ChunkSeq: &update.ChunkSeq, OccurredAt: time.Now().UTC(), Payload: payload})
	return nil
}
func tailText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	start := len(text) - limit
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}
func (w *WorkflowAgent) toolOutputLocked(ctx context.Context, scope agent.ExecutionScope, raw json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.active.ctx.Err() != nil || w.state.Run.ExecutionStopped {
		return product.NewError(product.CodeStateConflict, "tool output has stopped")
	}
	var update agent.ToolOutputFact
	if err := decode(raw, &update); err != nil {
		return err
	}
	c, ok := w.state.Calls[update.CallID]
	if !ok || !c.Claimed || c.Observation != nil || c.Scope.NodeExecutionID != scope.NodeExecutionID || update.ToolCallID != update.CallID || update.StreamID == "" || update.ChunkSeq == 0 || update.ChunkSeq != w.streamSeq[update.StreamID]+1 || !utf8.ValidString(update.Text) || len(update.Text) > config.ToolOutputChunkBytes {
		return product.NewError(product.CodeStateConflict, "tool output is not an active claimed stream")
	}
	if current := w.callStreams[update.CallID]; current != "" && current != update.StreamID {
		return product.NewError(product.CodeStateConflict, "tool output stream identity changed")
	}
	if update.Stream != "output" && update.Stream != "stdout" && update.Stream != "stderr" {
		return product.NewError(product.CodeInvalidArgument, "tool output stream is invalid")
	}
	w.callStreams[update.CallID] = update.StreamID
	key := update.ToolCallID + "\x00" + update.Stream
	previous := w.transient.Tools[key]
	previous.ToolCallID = update.ToolCallID
	previous.Stream = update.Stream
	text := previous.Text + update.Text
	previous.Truncated = previous.Truncated || len(text) > config.TransientToolPreviewBytes
	previous.Text = tailText(text, config.TransientToolPreviewBytes)
	w.previewOrder++
	previous.order = w.previewOrder
	w.transient.Tools[key] = previous
	w.streamSeq[update.StreamID] = update.ChunkSeq
	w.boundTransientLocked()
	payload, _ := json.Marshal(update.ToolOutputDelta)
	w.publishLocked(agent.Event{SchemaVersion: 1, Type: "tool.output.delta", Scope: w.eventScope(scope.NodeExecutionID), StreamID: update.StreamID, ChunkSeq: &update.ChunkSeq, OccurredAt: time.Now().UTC(), Payload: payload})
	return nil
}
func (w *WorkflowAgent) boundTransientLocked() {
	total := 0
	for _, m := range w.transient.Models {
		for _, b := range m.Blocks {
			total += len(b.Text) + len(b.Arguments)
		}
	}
	for _, t := range w.transient.Tools {
		total += len(t.Text)
	}
	for total > config.TransientSessionBytes {
		removed := false
		oldest := ""
		var order uint64
		for id, t := range w.transient.Tools {
			if oldest == "" || t.order < order {
				oldest, order = id, t.order
			}
		}
		if oldest != "" {
			total -= len(w.transient.Tools[oldest].Text)
			delete(w.transient.Tools, oldest)
			removed = true
		}
		if removed {
			continue
		}
		for id, m := range w.transient.Models {
			for _, b := range m.Blocks {
				total -= len(b.Text) + len(b.Arguments)
			}
			delete(w.transient.Models, id)
			removed = true
			break
		}
		if !removed {
			break
		}
	}
}
func (w *WorkflowAgent) clearTransientLocked() {
	w.transient = WorkflowTransient{Models: map[string]agent.ModelStreamSnapshot{}, Tools: map[string]WorkflowToolPreview{}}
	w.streamSeq = map[string]uint64{}
	w.callStreams = map[string]string{}
}
