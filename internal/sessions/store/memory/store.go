package memory

import (
	"context"
	product "github.com/ww1489/seasprak/internal/errors"
	"sync"

	"github.com/ww1489/seasprak/internal/sessions/store"
)

// Store keeps one session in memory and does not survive process restart.
type Store struct {
	mu     sync.Mutex
	id     string
	closed bool
	header store.Header
	chain  *store.Chain
	blobs  map[string][]byte
}

func Open(sessionID string, header store.Header) (*Store, error) {
	if err := store.ValidateSessionID(sessionID); err != nil {
		return nil, err
	}
	header.RecordType = "header"
	header.FormatVersion = 1
	header.SessionID = sessionID
	return &Store{id: sessionID, header: header, chain: store.NewChain()}, nil
}

func (s *Store) Load(ctx context.Context, sessionID string) (store.StoredSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := callContext(ctx); err != nil {
		return store.StoredSession{}, err
	}
	if err := s.ready(); err != nil {
		return store.StoredSession{}, err
	}
	if sessionID != s.id {
		return store.StoredSession{}, product.NewError(product.CodeNotFound, "session mismatch")
	}
	return s.chain.Session(s.header, false), nil
}

func (s *Store) Append(ctx context.Context, sessionID string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := callContext(ctx); err != nil {
		return store.CommitReceipt{}, err
	}
	if err := s.ready(); err != nil {
		return store.CommitReceipt{}, err
	}
	if sessionID != s.id {
		return store.CommitReceipt{}, product.NewError(product.CodeNotFound, "session mismatch")
	}
	sealed, receipt, duplicate, err := s.chain.Prepare(expected, commit)
	if err != nil || duplicate {
		return receipt, err
	}
	if err := s.chain.Apply(sealed); err != nil {
		return store.CommitReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) ReadAfter(ctx context.Context, sessionID string, after uint64) (store.CommitReader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := callContext(ctx); err != nil {
		return nil, err
	}
	if err := s.ready(); err != nil {
		return nil, err
	}
	if sessionID != s.id {
		return nil, product.NewError(product.CodeNotFound, "session mismatch")
	}
	return store.ReadCommits(s.chain.Session(s.header, false).Commits, after), nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func callContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func (s *Store) ready() error {
	if s.closed {
		return product.NewError(product.CodeStateConflict, "session store is closed")
	}
	return nil
}
