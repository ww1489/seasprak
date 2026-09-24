package store

import (
	"bytes"

	product "github.com/ww1489/seasprak/internal/errors"
)

// Chain is the shared commit sequence for the memory and JSONL stores.
type Chain struct {
	last      uint64
	cursor    uint64
	commits   []Commit
	commitIDs map[string]struct{}
	entryIDs  map[string]struct{}
	eventIDs  map[string]struct{}
	idem      map[string]idemHit
}

type idemHit struct {
	digest  []byte
	receipt CommitReceipt
}

func NewChain() *Chain {
	return &Chain{
		commitIDs: map[string]struct{}{},
		entryIDs:  map[string]struct{}{},
		eventIDs:  map[string]struct{}{},
		idem:      map[string]idemHit{},
	}
}

// Prepare checks an append and returns the commit that should be persisted.
// duplicate is true when the idempotency key already names the same digest.
func (c *Chain) Prepare(expected ExpectedCommit, in Commit) (Commit, CommitReceipt, bool, error) {
	if expected.IdempotencyKey != "" {
		if prior, ok := c.idem[expected.IdempotencyKey]; ok {
			if !bytes.Equal(prior.digest, expected.ContentDigest) {
				return Commit{}, CommitReceipt{}, false, product.NewError(product.CodeIdempotencyConflict, "idempotency key was used for different content")
			}
			receipt := CloneReceipt(prior.receipt)
			receipt.Duplicate = true
			return Commit{}, receipt, true, nil
		}
	}
	if expected.ExpectedPreviousSeq != c.last || in.ExpectedPreviousSeq != c.last {
		return Commit{}, CommitReceipt{}, false, product.NewError(product.CodeStateConflict, "commit sequence conflict")
	}
	commit := CloneCommit(in)
	commit.RecordType = "commit"
	commit.Version = 1
	commit.CommitSeq = c.last + 1
	commit.ExpectedPreviousSeq = c.last
	commit.IdempotencyKey = expected.IdempotencyKey
	commit.ContentDigest = cloneBytes(expected.ContentDigest)
	var seqs []uint64
	for i := range commit.Events {
		n := c.cursor + uint64(i) + 1
		commit.Events[i].DurableSeq = &n
		commit.Events[i].SchemaVersion = 1
		seqs = append(seqs, n)
	}
	if err := c.check(commit, false, product.CodeInvalidArgument); err != nil {
		return Commit{}, CommitReceipt{}, false, err
	}
	receipt := CommitReceipt{CommitID: commit.CommitID, CommitSeq: commit.CommitSeq, DurableSeq: append([]uint64(nil), seqs...)}
	return commit, receipt, false, nil
}

// Apply records a commit that was already validated for replay or a durable append.
func (c *Chain) Apply(commit Commit) error {
	return c.check(commit, true, product.CodeIncompatibleVersion)
}

func (c *Chain) Session(header Header, repair bool) StoredSession {
	return CloneSession(StoredSession{
		Header:         header,
		Commits:        c.commits,
		LastSeq:        c.last,
		RepairRequired: repair,
		DurableCursor:  c.cursor,
	})
}

func (c *Chain) check(commit Commit, mutate bool, code string) error {
	if commit.RecordType != "commit" {
		return product.NewError(code, "unknown record type")
	}
	if commit.Version != 1 {
		return product.NewError(code, "unknown commit version")
	}
	if commit.CommitID == "" {
		return product.NewError(code, "commit id is required")
	}
	if _, ok := c.commitIDs[commit.CommitID]; ok {
		return product.NewError(code, "duplicate commit id")
	}
	if commit.CommitSeq != c.last+1 || commit.ExpectedPreviousSeq != c.last {
		return product.NewError(code, "commit sequence does not continue")
	}
	var entryAdd []string
	for _, rec := range commit.Entries {
		if rec.Version != 1 || rec.Type == "" || rec.ID == "" {
			return product.NewError(code, "entry is missing type, version, or id")
		}
		if _, ok := c.entryIDs[rec.ID]; ok || containsID(entryAdd, rec.ID) {
			return product.NewError(code, "duplicate entry id")
		}
		if rec.ParentID != "" && !c.hasEntry(rec.ParentID) && !containsID(entryAdd, rec.ParentID) {
			return product.NewError(code, "entry parent is missing")
		}
		entryAdd = append(entryAdd, rec.ID)
	}
	for _, rec := range commit.ControlRecords {
		if rec.Version != 1 || rec.Type == "" || rec.ID == "" {
			return product.NewError(code, "control record is missing type, version, or id")
		}
	}
	cursor := c.cursor
	var eventAdd []string
	for _, ev := range commit.Events {
		if ev.SchemaVersion != 1 || ev.EventID == "" {
			return product.NewError(code, "event id or schema is invalid")
		}
		if _, ok := c.eventIDs[ev.EventID]; ok || containsID(eventAdd, ev.EventID) {
			return product.NewError(code, "duplicate event id")
		}
		if ev.DurableSeq == nil || *ev.DurableSeq != cursor+1 {
			return product.NewError(code, "durable sequence does not continue")
		}
		cursor = *ev.DurableSeq
		eventAdd = append(eventAdd, ev.EventID)
	}
	if commit.IdempotencyKey != "" {
		if _, ok := c.idem[commit.IdempotencyKey]; ok {
			return product.NewError(code, "duplicate idempotency key")
		}
	}
	if !mutate {
		return nil
	}
	c.last = commit.CommitSeq
	c.cursor = cursor
	c.commitIDs[commit.CommitID] = struct{}{}
	for _, id := range entryAdd {
		c.entryIDs[id] = struct{}{}
	}
	for _, id := range eventAdd {
		c.eventIDs[id] = struct{}{}
	}
	if commit.IdempotencyKey != "" {
		var seqs []uint64
		for _, ev := range commit.Events {
			seqs = append(seqs, *ev.DurableSeq)
		}
		c.idem[commit.IdempotencyKey] = idemHit{
			digest:  cloneBytes(commit.ContentDigest),
			receipt: CommitReceipt{CommitID: commit.CommitID, CommitSeq: commit.CommitSeq, DurableSeq: seqs},
		}
	}
	c.commits = append(c.commits, CloneCommit(commit))
	return nil
}

func (c *Chain) hasEntry(id string) bool {
	_, ok := c.entryIDs[id]
	return ok
}

func containsID(ids []string, id string) bool {
	for _, item := range ids {
		if item == id {
			return true
		}
	}
	return false
}

// ReadCommits returns commits whose commitSeq is greater than after.
func ReadCommits(commits []Commit, after uint64) CommitReader {
	out := make([]Commit, 0)
	for _, commit := range commits {
		if commit.CommitSeq > after {
			out = append(out, CloneCommit(commit))
		}
	}
	return &sliceReader{items: out}
}
