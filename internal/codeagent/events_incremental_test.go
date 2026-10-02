package codeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
)

type eventMailboxStore struct {
	store.Store
	fail  bool
	calls int
}

func (s *eventMailboxStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	s.calls++
	if s.fail {
		return store.CommitReceipt{}, errors.New("injected append failure")
	}
	return s.Store.Append(ctx, id, expected, c)
}
func eventMailbox(t testing.TB, history, payloadBytes int) (*AgentSession, *eventMailboxStore) {
	t.Helper()
	base, err := memory.Open("events", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	backend := &eventMailboxStore{Store: base}
	if history > 0 {
		payload, _ := json.Marshal(strings.Repeat("界", payloadBytes/3))
		events := make([]agent.Event, history)
		for i := range events {
			events[i] = agent.Event{SchemaVersion: 1, Type: "history", EventID: fmt.Sprintf("history-%d", i), Scope: agent.EventScope{SessionID: "events"}, Payload: payload}
		}
		if _, err := backend.Append(context.Background(), "events", store.ExpectedCommit{}, store.Commit{CommitID: "history", Events: events}); err != nil {
			t.Fatal(err)
		}
	}
	m, err := state.NewManager(backend, "events")
	if err != nil {
		t.Fatal(err)
	}
	rt := &runtime{manager: m, opts: Options{Store: backend}, mailbox: make(chan command), done: make(chan struct{}), subs: map[int]*subscription{}}
	go rt.loop()
	t.Cleanup(func() {
		_ = rt.do(context.Background(), func(rt *runtime) error { rt.closing = true; return nil })
		<-rt.done
	})
	// Runs the actual mailbox's publish step, with no subscribers present.
	if err := rt.do(context.Background(), func(*runtime) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return &AgentSession{rt: rt}, backend
}
func eventMailboxAccept(t *testing.T, s *AgentSession) agent.InputReceipt {
	t.Helper()
	var receipt agent.InputReceipt
	if err := s.rt.do(t.Context(), func(rt *runtime) error {
		var err error
		receipt, err = rt.manager.Accept(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"fixture"}`)}, agent.TargetAgent{Name: "main", Generation: "gen"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return receipt
}
func receiveCommitted(t *testing.T, sub *Subscription) agent.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events:
		if !ok {
			t.Fatalf("subscription closed: %v", sub.Err())
		}
		return ev
	case <-time.After(time.Second):
		t.Fatal("committed event missing")
	}
	return agent.Event{}
}
func assertNoQueuedEvents(t *testing.T, s *AgentSession, sub *Subscription) {
	t.Helper()
	if err := s.rt.do(t.Context(), func(*runtime) error { return nil }); err != nil {
		t.Fatal(err)
	}
	sub.sub.mu.Lock()
	defer sub.sub.mu.Unlock()
	if len(sub.sub.queue) != 0 {
		t.Fatalf("unexpected queued events: %d", len(sub.sub.queue))
	}
}

func TestCommittedEventMailboxIsolationOrderingAndFailure(t *testing.T) {
	s, backend := eventMailbox(t, 2, 1024)
	first := s.SubscribeEvents(config.DefaultLimits())
	defer first.Close()
	second := s.SubscribeEvents(config.DefaultLimits())
	defer second.Close()
	assertNoQueuedEvents(t, s, first)
	r := eventMailboxAccept(t, s)
	a, b := receiveCommitted(t, first), receiveCommitted(t, second)
	if a.DurableSeq == nil || b.DurableSeq == nil || *a.DurableSeq != 3 || *b.DurableSeq != 3 {
		t.Fatal("old history replayed or event sequence changed")
	}
	original := string(b.Payload)
	a.Payload[0] = '!'
	*a.DurableSeq = 999
	a.Scope.TraceID = "mutated"
	if string(b.Payload) != original || *b.DurableSeq != 3 {
		t.Fatal("subscriber mutation crossed subscription boundary")
	}
	view := s.rt.manager.View()
	if string(view.Events[2].Payload) != original || *view.Events[2].DurableSeq != 3 || view.Events[2].Scope.TraceID != r.TraceID {
		t.Fatal("subscriber mutation polluted state")
	}
	late := s.SubscribeEvents(config.DefaultLimits())
	defer late.Close()
	assertNoQueuedEvents(t, s, late)
	if err := s.rt.do(t.Context(), func(rt *runtime) error { return rt.manager.SetTraceState(t.Context(), r.TraceID, "running", false) }); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []*Subscription{first, second, late} {
		ev := receiveCommitted(t, sub)
		if ev.DurableSeq == nil || *ev.DurableSeq != 4 {
			t.Fatal("incremental event missing or duplicated")
		}
	}
	// Wait for delivery queue bookkeeping; this adds no production retries.
	for _, sub := range []*Subscription{first, second, late} {
		waitResumeCondition(t, func() bool { sub.sub.mu.Lock(); defer sub.sub.mu.Unlock(); return len(sub.sub.queue) == 0 })
	}
	if err := s.rt.do(t.Context(), func(rt *runtime) error {
		_, err := rt.manager.ReserveActivity(t.Context(), r.TraceID, "execution", 0, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []*Subscription{first, second, late} {
		assertNoQueuedEvents(t, s, sub)
	}
	before := backend.calls
	err := s.rt.do(t.Context(), func(rt *runtime) error {
		backend.fail = true
		return rt.manager.SetTraceState(t.Context(), r.TraceID, "completed", true)
	})
	if err == nil || backend.calls != before+1 {
		t.Fatal("append failure not exercised exactly once")
	}
	for _, sub := range []*Subscription{first, second, late} {
		assertNoQueuedEvents(t, s, sub)
	}
	if err := s.rt.do(t.Context(), func(rt *runtime) error {
		if rt.cursor != 4 {
			return fmt.Errorf("cursor=%d", rt.cursor)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedEventMailboxSlowSubscriberOverflow(t *testing.T) {
	s, _ := eventMailbox(t, 0, 0)
	limits := config.DefaultLimits()
	limits.SubscriptionEvents = 1
	slow := s.SubscribeEvents(limits)
	defer slow.Close()
	fast := s.SubscribeEvents(config.DefaultLimits())
	defer fast.Close()
	r := eventMailboxAccept(t, s)
	if ev := receiveCommitted(t, fast); ev.DurableSeq == nil || *ev.DurableSeq != 1 {
		t.Fatal("first sequence missing")
	}
	if err := s.rt.do(t.Context(), func(rt *runtime) error { return rt.manager.SetTraceState(t.Context(), r.TraceID, "running", false) }); err != nil {
		t.Fatal(err)
	}
	pe, ok := product.AsError(slow.Err())
	if !ok || pe.Code != product.CodeResyncRequired {
		t.Fatalf("overflow=%v", slow.Err())
	}
	if ev := receiveCommitted(t, fast); ev.DurableSeq == nil || *ev.DurableSeq != 2 {
		t.Fatal("slow subscriber interrupted other delivery")
	}
}

func TestCommittedEventMailboxCaughtUpDoesNotCloneHistory(t *testing.T) {
	small, _ := eventMailbox(t, 1, 1024)
	large, _ := eventMailbox(t, 256, 1024)
	measure := func(s *AgentSession) float64 {
		return testing.AllocsPerRun(3, func() {
			if err := s.rt.do(context.Background(), func(*runtime) error { return nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
	smallAllocs, largeAllocs := measure(small), measure(large)
	t.Logf("caught-up mailbox allocations: one_event=%g events_256=%g", smallAllocs, largeAllocs)
	// An allocation bound, not a wall-clock threshold. Copying the historical
	// payloads and sequence pointers necessarily grows by hundreds of objects.
	if largeAllocs > smallAllocs+128 {
		t.Fatal("caught-up event publication still clones historical state")
	}
}

func BenchmarkCommittedEventMailboxCaughtUp(b *testing.B) {
	for _, pages := range []int{1, 8, 32, 128} {
		b.Run(fmt.Sprintf("pages_%d", pages), func(b *testing.B) {
			s, _ := eventMailbox(b, pages, 50*1024)
			b.ReportAllocs()
			for b.Loop() {
				if err := s.rt.do(context.Background(), func(*runtime) error { return nil }); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
