package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

var _ store.CheckpointBlobs = (*Store)(nil)

func (s *Store) Put(ctx context.Context, sessionID string, data []byte) (store.BlobRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.blobReady(ctx, sessionID); err != nil {
		return store.BlobRef{}, err
	}
	data = bytes.Clone(data)
	ref := store.BlobRef{Hash: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data))}
	if old, ok := s.blobs[ref.Hash]; ok {
		if err := store.VerifyBlob(ref, old); err != nil {
			return store.BlobRef{}, err
		}
	} else {
		if s.blobs == nil {
			s.blobs = make(map[string][]byte)
		}
		s.blobs[ref.Hash] = data
	}
	return ref, nil
}
func (s *Store) Get(ctx context.Context, sessionID string, ref store.BlobRef) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.blobReady(ctx, sessionID); err != nil {
		return nil, err
	}
	if err := store.ValidateBlobRef(ref); err != nil {
		return nil, err
	}
	data, ok := s.blobs[ref.Hash]
	if !ok {
		return nil, product.NewError(product.CodeNotFound, "checkpoint not found")
	}
	if err := store.VerifyBlob(ref, data); err != nil {
		return nil, err
	}
	return bytes.Clone(data), nil
}
func (s *Store) blobReady(ctx context.Context, sessionID string) error {
	if err := callContext(ctx); err != nil {
		return err
	}
	if err := s.ready(); err != nil {
		return err
	}
	if sessionID != s.id {
		return product.NewError(product.CodeNotFound, "session mismatch")
	}
	return nil
}
