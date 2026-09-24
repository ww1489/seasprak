package memory

import (
	"context"
	"sync"

	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/model"
	"github.com/ww1489/seasprak/session/history"
)

// Store keeps one session in memory and does not survive process restart.
type Store struct {
	mu     sync.Mutex
	id     string
	closed bool
	header history.Header
	chain  *history.Chain
}

func Open(sessionID string, header history.Header) (*Store, error) {
	if err := storage.ValidateSessionID(sessionID); err != nil {
		return nil, err
	}
	header.RecordType = "header"
	header.FormatVersion = 1
	header.SessionID = sessionID
	return &Store{id: sessionID, header: header, chain: history.NewChain()}, nil
}

func (s *Store) Load(ctx context.Context, sessionID string) (history.StoredSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := callContext(ctx); err != nil {
		return history.StoredSession{}, err
	}
	if err := s.ready(); err != nil {
		return history.StoredSession{}, err
	}
	if sessionID != s.id {
		return history.StoredSession{}, model.NewError(model.CodeNotFound, "session mismatch")
	}
	return s.chain.Session(s.header, false), nil
}

func (s *Store) Append(ctx context.Context, sessionID string, expected history.ExpectedCommit, commit history.Commit) (history.CommitReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := callContext(ctx); err != nil {
		return history.CommitReceipt{}, err
	}
	if err := s.ready(); err != nil {
		return history.CommitReceipt{}, err
	}
	if sessionID != s.id {
		return history.CommitReceipt{}, model.NewError(model.CodeNotFound, "session mismatch")
	}
	sealed, receipt, duplicate, err := s.chain.Prepare(expected, commit)
	if err != nil || duplicate {
		return receipt, err
	}
	if err := s.chain.Apply(sealed); err != nil {
		return history.CommitReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) ReadAfter(ctx context.Context, sessionID string, after uint64) (history.CommitReader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := callContext(ctx); err != nil {
		return nil, err
	}
	if err := s.ready(); err != nil {
		return nil, err
	}
	if sessionID != s.id {
		return nil, model.NewError(model.CodeNotFound, "session mismatch")
	}
	return history.ReadCommits(s.chain.Session(s.header, false).Commits, after), nil
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
		return model.NewError(model.CodeStateConflict, "session store is closed")
	}
	return nil
}
