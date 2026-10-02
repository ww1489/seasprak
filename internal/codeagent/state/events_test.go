package state

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	store "github.com/ww1489/seasprak/internal/storage"
)

type eventReadStore struct {
	store.Store
	chain *store.Chain
	fail  bool
	calls int
}

func (s *eventReadStore) Append(_ context.Context, _ string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	s.calls++
	if s.fail {
		return store.CommitReceipt{}, errors.New("injected append failure")
	}
	sealed, receipt, duplicate, err := s.chain.Prepare(expected, c)
	if err != nil || duplicate {
		return receipt, err
	}
	return receipt, s.chain.Apply(sealed)
}
func (s *eventReadStore) Load(context.Context, string) (store.StoredSession, error) {
	return s.chain.Session(store.Header{}, false), nil
}

func TestCommittedEventsAfterVisibilityCursorAndIsolation(t *testing.T) {
	backend := &eventReadStore{chain: store.NewChain()}
	m, err := NewManager(backend, "events")
	if err != nil {
		t.Fatal(err)
	}
	reader := m
	if ev, cursor := reader.EventsAfter(0); len(ev) != 0 || cursor != 0 {
		t.Fatalf("initial count=%d cursor=%d", len(ev), cursor)
	}
	chunk := uint64(7)
	first := agent.Event{SchemaVersion: 1, Type: "first", EventID: "event-1", Scope: agent.EventScope{SessionID: "events", TraceID: "trace", TurnID: "turn"}, ChunkSeq: &chunk, Payload: []byte(`{"nested":{"value":1}}`)}
	second := first
	second.EventID, second.Type = "event-2", "second"
	if _, err := m.commit(t.Context(), nil, nil, []agent.Event{first, second}); err != nil {
		t.Fatal(err)
	}
	events, cursor := reader.EventsAfter(0)
	if len(events) != 2 || cursor != 2 || *events[0].DurableSeq != 1 || *events[1].DurableSeq != 2 {
		t.Fatalf("count=%d cursor=%d", len(events), cursor)
	}
	original := clone(events)
	events[0].Payload[0] = '!'
	*events[0].DurableSeq = 999
	*events[0].ChunkSeq = 999
	events[0].Scope.TraceID = "mutated"
	events[1] = agent.Event{}
	again, cursor := reader.EventsAfter(0)
	if cursor != 2 || !reflect.DeepEqual(again, original) {
		t.Fatal("returned event mutation polluted committed state")
	}
	for _, after := range []uint64{0, 1, 2, 999} {
		got, current := reader.EventsAfter(after)
		want := original
		if after == 1 {
			want = original[1:]
		}
		if after >= 2 {
			want = nil
		}
		if len(got) != len(want) || current != 2 {
			t.Fatalf("after=%d count=%d cursor=%d", after, len(got), current)
		}
		if len(want) > 0 && !reflect.DeepEqual(got, want) {
			t.Fatal("event order or content changed")
		}
	}
	if _, err := m.commit(t.Context(), nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, current := reader.EventsAfter(2); len(got) != 0 || current != 2 || m.view.LastSeq != 2 {
		t.Fatal("eventless commit advanced event cursor")
	}
	backend.fail = true
	third := first
	third.EventID = "event-3"
	if _, err := m.commit(t.Context(), nil, nil, []agent.Event{third}); err == nil {
		t.Fatal("append failure missing")
	}
	if got, current := reader.EventsAfter(2); len(got) != 0 || current != 2 || backend.calls != 3 {
		t.Fatalf("failed append became visible; calls=%d", backend.calls)
	}
	reopened, err := NewManager(backend, "events")
	if err != nil {
		t.Fatal(err)
	}
	got, current := reopened.EventsAfter(1)
	if current != 2 || !reflect.DeepEqual(got, original[1:]) {
		t.Fatal("replay changed event suffix or cursor")
	}
}
