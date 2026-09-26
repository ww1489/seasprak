package eino

import (
	"bytes"
	"context"
	"encoding/gob"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

// CheckpointStore binds Eino's ephemeral key to an immutable product blob.
// It never removes blobs on Delete; committed session references may still need them.
type CheckpointStore struct {
	mu    sync.Mutex
	blobs agent.CheckpointBlobPort
	keys  map[string]agent.CheckpointBlobRef
}

var _ adk.CheckPointStore = (*CheckpointStore)(nil)

func NewCheckpointStore(blobs agent.CheckpointBlobPort) *CheckpointStore {
	return &CheckpointStore{blobs: blobs, keys: make(map[string]agent.CheckpointBlobRef)}
}
func (s *CheckpointStore) Bind(key string, ref agent.CheckpointBlobRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[key] = ref
}

func (s *CheckpointStore) Set(ctx context.Context, key string, value []byte) error {
	if s == nil || s.blobs == nil || key == "" {
		return product.NewError(product.CodeStorageUnavailable, "checkpoint blob storage is unavailable")
	}
	ref, err := s.blobs.PutCheckpoint(ctx, value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.keys[key] = ref
	s.mu.Unlock()
	return nil
}
func (s *CheckpointStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	ref, ok := s.keys[key]
	s.mu.Unlock()
	if !ok {
		return nil, false, nil
	}
	data, err := s.blobs.GetCheckpoint(ctx, ref)
	return data, err == nil, err
}
func (s *CheckpointStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	delete(s.keys, key)
	s.mu.Unlock()
	return nil
}
func (s *CheckpointStore) Ref(key string) (agent.CheckpointBlobRef, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.keys[key]
	return ref, ok
}

// ValidatePausedCheckpoint validates the pinned Eino v0.9.21 gob envelope.
// Neither blob presence nor a between-turn checkpoint authorizes a paused trace.
func ValidatePausedCheckpoint(data []byte, expected agent.InputRef) error {
	var cp struct {
		RunnerCheckpoint []byte
		HasRunnerState   bool
		UnhandledItems   []agent.InputRef
		CanceledItems    []agent.InputRef
	}
	r := bytes.NewReader(data)
	if err := gob.NewDecoder(r).Decode(&cp); err != nil {
		return product.NewError(product.CodeIncompatibleResume, "invalid Eino checkpoint envelope")
	}
	if r.Len() != 0 || !cp.HasRunnerState || len(cp.RunnerCheckpoint) == 0 || len(cp.CanceledItems) != 1 || cp.CanceledItems[0] != expected || len(cp.UnhandledItems) != 0 {
		return product.NewError(product.CodeIncompatibleResume, "checkpoint has no matching interrupted runner state")
	}
	// The runner checkpoint itself is framework-private and must not be interpreted by sessions.
	return nil
}
