package sessions

import (
	"context"
	"encoding/json"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"sync"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

type Snapshot struct {
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
