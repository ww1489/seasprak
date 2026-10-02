package eino

import (
	"bytes"
	"context"
	"sync"
)

// Shared test-only serialized store for the ordinary native child checkpoint
// probes. It owns no workflow definition, graph or product recovery state.
type p3WorkflowStore struct {
	mu             sync.Mutex
	data           map[string][]byte
	gets, sets     int
	getErr, setErr error
}

func (s *p3WorkflowStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.getErr != nil {
		return nil, false, s.getErr
	}
	data, ok := s.data[key]
	return bytes.Clone(data), ok, nil
}
func (s *p3WorkflowStore) Set(_ context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets++
	if s.setErr != nil {
		return s.setErr
	}
	if s.data == nil {
		s.data = make(map[string][]byte)
	}
	s.data[key] = bytes.Clone(data)
	return nil
}
func (s *p3WorkflowStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.sets
}
