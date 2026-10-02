package codeagent

import (
	"context"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

const replayPageEvents = 256

// ReplaySubscription delivers durable events after a cursor, then live events,
// with no gap or duplicate at the handoff point. Err reports resync_required
// when the live buffer overflowed while history was replaying.
type ReplaySubscription struct {
	Events <-chan agent.Event
	// Handoff is the durable cursor fixed when the subscription registered.
	Handoff uint64
	live    *Subscription
	done    chan struct{}
	stopped chan struct{}
}

func (r *ReplaySubscription) Close() {
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	<-r.stopped
	r.live.Close()
}

// Err is valid after Events is closed.
func (r *ReplaySubscription) Err() error { return r.live.Err() }

// SubscribeFrom fixes handoff point B inside one mailbox operation: committed
// events up to B are published, and a live subscription that only receives
// events after B is registered. History (after, B] is then paged outside the
// mailbox, so replay never blocks the session and never enters the live queue.
func (s *AgentSession) SubscribeFrom(ctx context.Context, after uint64, limits config.Limits) (*ReplaySubscription, error) {
	return s.subscribeFrom(ctx, after, limits, false)
}

// subscribeFrom with historyOnly ends the stream at the handoff point; it is
// used for read-only sessions that can never commit later events.
func (s *AgentSession) subscribeFrom(ctx context.Context, after uint64, limits config.Limits, historyOnly bool) (*ReplaySubscription, error) {
	limits = limits.WithDefaults()
	live := &subscription{ch: make(chan agent.Event), wake: make(chan struct{}, 1), done: make(chan struct{}), stopped: make(chan struct{}), limits: limits}
	var handoff uint64
	err := s.rt.do(ctx, func(rt *runtime) error {
		if rt.closing {
			return product.NewError(product.CodeStateConflict, "session is closing")
		}
		rt.publishCommitted()
		handoff = rt.cursor
		if after > handoff {
			return product.NewError(product.CodeInvalidArgument, "cursor is ahead of the session")
		}
		rt.nextSub++
		live.id = rt.nextSub
		rt.subs[live.id] = live
		return nil
	})
	if err != nil {
		live.close(err)
		close(live.stopped)
		return nil, err
	}
	go live.deliver()
	liveSub := &Subscription{Events: live.ch, sub: live, cancel: func() {
		live.close(nil)
		<-live.stopped
		_ = s.rt.do(context.Background(), func(rt *runtime) error { delete(rt.subs, live.id); return nil })
	}}
	out := make(chan agent.Event)
	r := &ReplaySubscription{Events: out, Handoff: handoff, live: liveSub, done: make(chan struct{}), stopped: make(chan struct{})}
	go r.run(s, after, handoff, historyOnly, out)
	return r, nil
}

func (r *ReplaySubscription) run(s *AgentSession, after, handoff uint64, historyOnly bool, out chan<- agent.Event) {
	defer close(r.stopped)
	defer close(out)
	for after < handoff {
		page := s.rt.manager.EventsRange(after, handoff, replayPageEvents)
		if len(page) == 0 {
			break
		}
		for _, ev := range page {
			select {
			case out <- ev:
			case <-r.done:
				return
			}
			after = *ev.DurableSeq
		}
	}
	if historyOnly {
		return
	}
	for {
		select {
		case ev, ok := <-r.live.Events:
			if !ok {
				return
			}
			select {
			case out <- ev:
			case <-r.done:
				return
			}
		case <-r.done:
			return
		}
	}
}
