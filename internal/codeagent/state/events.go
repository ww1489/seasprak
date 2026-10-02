package state

import (
	"sort"

	"github.com/ww1489/seasprak/internal/agent"
)

// EventsAfter returns an isolated suffix of committed durable events and the
// cursor from the same locked view. A cursor at or beyond the committed cursor
// has no suffix. Commits without events do not advance this cursor.
func (m *Manager) EventsAfter(after uint64) ([]agent.Event, uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cursor := m.view.Cursor
	if after >= cursor {
		return nil, cursor
	}
	// Manager.commit assigns consecutive durable sequences; store validation
	// preserves that order on replay. Transient events never enter View.Events.
	events := m.view.Events
	first := sort.Search(len(events), func(i int) bool { return *events[i].DurableSeq > after })
	return clone(events[first:]), cursor
}

// EventsRange returns at most max isolated durable events with sequence in
// (after, upto]. Paging a fixed upper bound never observes later commits.
func (m *Manager) EventsRange(after, upto uint64, max int) []agent.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	if after >= upto || max <= 0 {
		return nil
	}
	events := m.view.Events
	first := sort.Search(len(events), func(i int) bool { return *events[i].DurableSeq > after })
	last := first
	for last < len(events) && last-first < max && *events[last].DurableSeq <= upto {
		last++
	}
	return clone(events[first:last])
}
