package history

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/ww1489/seasprak/agent"
)

type Record struct {
	Type     string          `json:"type"`
	Version  int             `json:"version"`
	ID       string          `json:"id"`
	ParentID string          `json:"parentId,omitempty"`
	Payload  json.RawMessage `json:"payload"`
}

type BranchUpdate struct {
	BranchID string `json:"branchId"`
	LeafID   string `json:"leafId"`
	Active   bool   `json:"active"`
}

type Commit struct {
	RecordType          string         `json:"recordType"`
	Version             int            `json:"version"`
	CommitID            string         `json:"commitId"`
	CommitSeq           uint64         `json:"commitSeq"`
	ExpectedPreviousSeq uint64         `json:"expectedPreviousSeq"`
	Entries             []Record       `json:"entries"`
	ControlRecords      []Record       `json:"controlRecords"`
	BranchUpdates       []BranchUpdate `json:"branchUpdates"`
	Events              []agent.Event  `json:"events"`
	IdempotencyKey      string         `json:"idempotencyKey,omitempty"`
	ContentDigest       []byte         `json:"contentDigest,omitempty"`
}

type ExpectedCommit struct {
	ExpectedPreviousSeq uint64
	IdempotencyKey      string
	ContentDigest       []byte
}

type CommitReceipt struct {
	CommitID   string   `json:"commitId"`
	CommitSeq  uint64   `json:"commitSeq"`
	DurableSeq []uint64 `json:"durableSeq"`
	Duplicate  bool     `json:"duplicate"`
}

type Header struct {
	RecordType    string          `json:"recordType"`
	FormatVersion int             `json:"formatVersion"`
	SessionID     string          `json:"sessionId"`
	CreatedAt     time.Time       `json:"createdAt"`
	Workspace     json.RawMessage `json:"workspace"`
}

type StoredSession struct {
	Header         Header
	Commits        []Commit
	LastSeq        uint64
	RepairRequired bool
	DurableCursor  uint64
}

type CommitReader interface {
	Next() (Commit, error)
}

type SessionStore interface {
	Load(ctx context.Context, sessionID string) (StoredSession, error)
	Append(ctx context.Context, sessionID string, expected ExpectedCommit, commit Commit) (CommitReceipt, error)
	ReadAfter(ctx context.Context, sessionID string, after uint64) (CommitReader, error)
	Close() error
}

type sliceReader struct {
	items []Commit
	i     int
}

func (r *sliceReader) Next() (Commit, error) {
	if r.i >= len(r.items) {
		return Commit{}, io.EOF
	}
	item := r.items[r.i]
	r.i++
	return item, nil
}
