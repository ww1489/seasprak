package sessions

import (
	"context"
	"encoding/json"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

type ResumeEligibility struct {
	CanResume bool   `json:"canResume"`
	Code      string `json:"code,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type Snapshot struct {
	Revision       uint64
	Resume         map[string]ResumeEligibility
	SessionID      string
	Cursor         uint64
	ActiveTrace    string
	Traces         map[string]*state.TraceState
	Inputs         map[string]*state.InputState
	Messages       []agent.AgentMessage
	Turns          map[string]agent.TurnRecord
	Calls          map[string]agent.ToolRecord
	RepairRequired bool
}
type queuedEvent struct {
	event agent.Event
	size  int
}
type subscription struct {
	id      int
	ch      chan agent.Event
	wake    chan struct{}
	done    chan struct{}
	stopped chan struct{}
	limits  config.Limits
	mu      sync.Mutex
	queue   []queuedEvent
	bytes   int
	closed  bool
	err     error
}

// Subscription exposes a terminal resync_required error when its pending buffer overflows.
type Subscription struct {
	Events <-chan agent.Event
	sub    *subscription
	cancel func()
}

func (s *Subscription) Close()     { s.cancel() }
func (s *Subscription) Err() error { s.sub.mu.Lock(); defer s.sub.mu.Unlock(); return s.sub.err }
func (s *AgentSession) Subscribe(limits config.Limits) (<-chan agent.Event, func()) {
	sub := s.SubscribeEvents(limits)
	return sub.Events, sub.Close
}
func (s *AgentSession) SubscribeEvents(limits config.Limits) *Subscription {
	limits = limits.WithDefaults()
	sub := &subscription{ch: make(chan agent.Event), wake: make(chan struct{}, 1), done: make(chan struct{}), stopped: make(chan struct{}), limits: limits}
	err := s.rt.do(context.Background(), func(rt *runtime) error {
		if rt.closing {
			return product.NewError(product.CodeStateConflict, "session is closing")
		}
		rt.nextSub++
		sub.id = rt.nextSub
		rt.subs[sub.id] = sub
		return nil
	})
	if err != nil {
		sub.close(err)
	}
	go sub.deliver()
	return &Subscription{Events: sub.ch, sub: sub, cancel: func() {
		sub.close(nil)
		<-sub.stopped
		_ = s.rt.do(context.Background(), func(rt *runtime) error { delete(rt.subs, sub.id); return nil })
	}}
}
func (sub *subscription) close(err error) {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if !sub.closed {
		sub.closed = true
		sub.err = err
		close(sub.done)
	}
}
func (sub *subscription) enqueue(ev agent.Event) bool {
	raw, err := json.Marshal(ev)
	if err != nil {
		sub.close(err)
		return false
	}
	var copied agent.Event
	if err := json.Unmarshal(raw, &copied); err != nil {
		sub.close(err)
		return false
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.closed {
		return false
	}
	if len(sub.queue) >= sub.limits.SubscriptionEvents || sub.bytes+len(raw) > sub.limits.SubscriptionBytes {
		sub.closed = true
		sub.err = product.NewError(product.CodeResyncRequired, "subscriber fell behind; reload a snapshot and resume from its cursor")
		close(sub.done)
		return false
	}
	sub.queue = append(sub.queue, queuedEvent{event: copied, size: len(raw)})
	sub.bytes += len(raw)
	select {
	case sub.wake <- struct{}{}:
	default:
	}
	return true
}
func (sub *subscription) deliver() {
	defer func() {
		if sub.stopped != nil {
			close(sub.stopped)
		}
	}()
	defer close(sub.ch)
	for {
		sub.mu.Lock()
		if sub.closed {
			sub.queue = nil
			sub.bytes = 0
			sub.mu.Unlock()
			return
		}
		if len(sub.queue) == 0 {
			sub.mu.Unlock()
			select {
			case <-sub.wake:
				continue
			case <-sub.done:
				return
			}
		}
		item := sub.queue[0]
		sub.mu.Unlock()
		select {
		case <-sub.done:
			return
		case sub.ch <- item.event:
		}
		sub.mu.Lock()
		sub.queue[0] = queuedEvent{}
		sub.queue = sub.queue[1:]
		sub.bytes -= item.size
		sub.mu.Unlock()
	}
}
func (rt *runtime) publishModelStarted(scope agent.ExecutionScope, identity agent.ModelAttemptIdentity) {
	if rt.modelChunks == nil {
		rt.modelChunks = map[string]uint64{}
	}
	rt.modelChunks[identity.StreamID] = 0
	payload, _ := json.Marshal(identity)
	seq := uint64(0)
	ev := agent.Event{SchemaVersion: 1, Type: "message.started", Scope: agent.EventScope{SessionID: scope.SessionID, TraceID: scope.TraceID, TurnID: scope.TurnID}, StreamID: identity.StreamID, ChunkSeq: &seq, OccurredAt: time.Now().UTC(), Payload: payload}
	for id, sub := range rt.subs {
		if !sub.enqueue(ev) {
			delete(rt.subs, id)
		}
	}
}

func (rt *runtime) saveAttemptResult(ctx context.Context, scope agent.ExecutionScope, id, status, finish string, msg *agent.AgentMessage, calls []agent.ToolRecord, details agent.ModelAttemptDetails) error {
	if err := rt.manager.SaveAttemptResult(ctx, scope, id, status, finish, msg, calls, details); err != nil {
		return err
	}
	delete(rt.modelChunks, rt.manager.View().ModelAttempts[id].StreamID)
	return nil
}

func (rt *runtime) publishModelSnapshot(ctx context.Context, scope agent.ExecutionScope, payload json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := rt.active.ctx.Err(); err != nil {
		return err
	}
	var update agent.ModelStreamSnapshot
	if err := json.Unmarshal(payload, &update); err != nil {
		return err
	}
	v := rt.manager.View()
	initial, ok := v.ModelAttempts[update.AttemptID]
	_, ended := v.AttemptResults[update.AttemptID]
	if !ok || ended || initial.Scope != scope || initial.MessageID != update.MessageID || initial.StreamID != update.StreamID || update.ChunkSeq == 0 || update.ChunkSeq <= rt.modelChunks[update.StreamID] {
		return product.NewError(product.CodeStateConflict, "model stream is not active or update is stale")
	}
	if rt.modelChunks == nil {
		rt.modelChunks = map[string]uint64{}
	}
	rt.modelChunks[update.StreamID] = update.ChunkSeq
	// Decode then re-encode the allowlist, so unknown fields cannot become public.
	raw, err := json.Marshal(update)
	if err != nil {
		return err
	}
	ev := agent.Event{SchemaVersion: 1, Type: "message.snapshot", Scope: agent.EventScope{SessionID: scope.SessionID, TraceID: scope.TraceID, TurnID: scope.TurnID}, StreamID: update.StreamID, ChunkSeq: &update.ChunkSeq, OccurredAt: time.Now().UTC(), Payload: raw}
	for id, sub := range rt.subs {
		if !sub.enqueue(ev) {
			delete(rt.subs, id)
		}
	}
	return nil
}

func (rt *runtime) publishToolOutput(ctx context.Context, scope agent.ExecutionScope, payload json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := rt.active.ctx.Err(); err != nil {
		return err
	}
	if !utf8.Valid(payload) {
		return product.NewError(product.CodeInvalidArgument, "tool output fact is not valid UTF-8")
	}
	var update agent.ToolOutputFact
	if err := json.Unmarshal(payload, &update); err != nil {
		return err
	}
	view := rt.manager.View()
	call, exists := view.Calls[update.CallID]
	trace := view.Traces[scope.TraceID]
	if !exists || trace == nil || trace.State != "running" || !call.Claimed || call.Observation != nil ||
		!acceptedAttemptForCall(view, call) || !rt.matchesCallScope(scope, call) || call.Call.CallID != update.ToolCallID ||
		update.StreamID == "" || update.ChunkSeq == 0 || update.Text == "" || len(update.Text) > config.ToolOutputChunkBytes ||
		(update.Stream != "output" && update.Stream != "stdout" && update.Stream != "stderr") {
		return product.NewError(product.CodeStateConflict, "tool output does not belong to an active claimed call")
	}
	position, exists := rt.active.toolChunks[update.CallID]
	if (!exists && update.ChunkSeq != 1) || (exists && (position.streamID != update.StreamID || update.ChunkSeq != position.seq+1)) {
		return product.NewError(product.CodeStateConflict, "tool output stream is stale")
	}
	if rt.active.toolChunks == nil {
		rt.active.toolChunks = make(map[string]toolChunkPosition)
	}
	rt.active.toolChunks[update.CallID] = toolChunkPosition{streamID: update.StreamID, seq: update.ChunkSeq}
	// Re-encode the public allowlist; the internal call identity and unknown
	// fact fields never enter the SDK event payload or the durable journal.
	public, err := json.Marshal(update.ToolOutputDelta)
	if err != nil {
		return err
	}
	ev := agent.Event{SchemaVersion: 1, Type: "tool.output.delta", Scope: agent.EventScope{SessionID: scope.SessionID, TraceID: scope.TraceID, TurnID: scope.TurnID}, StreamID: update.StreamID, ChunkSeq: &update.ChunkSeq, OccurredAt: time.Now().UTC(), Payload: public}
	for id, sub := range rt.subs {
		if !sub.enqueue(ev) {
			delete(rt.subs, id)
		}
	}
	return nil
}

func (rt *runtime) publishCommitted() {
	v := rt.manager.View()
	for _, ev := range v.Events {
		if ev.DurableSeq == nil || *ev.DurableSeq <= rt.cursor {
			continue
		}
		for id, sub := range rt.subs {
			if !sub.enqueue(ev) {
				delete(rt.subs, id)
			}
		}
		rt.cursor = *ev.DurableSeq
	}
}
