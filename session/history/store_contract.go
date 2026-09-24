package history

import (
	"bytes"
	"encoding/json"

	"github.com/ww1489/seasprak/agent"
	"github.com/ww1489/seasprak/model"
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
				return Commit{}, CommitReceipt{}, false, model.NewError(model.CodeIdempotencyConflict, "idempotency key was used for different content")
			}
			receipt := CloneReceipt(prior.receipt)
			receipt.Duplicate = true
			return Commit{}, receipt, true, nil
		}
	}
	if expected.ExpectedPreviousSeq != c.last || in.ExpectedPreviousSeq != c.last {
		return Commit{}, CommitReceipt{}, false, model.NewError(model.CodeStateConflict, "commit sequence conflict")
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
	if err := c.check(commit, false, model.CodeInvalidArgument); err != nil {
		return Commit{}, CommitReceipt{}, false, err
	}
	receipt := CommitReceipt{CommitID: commit.CommitID, CommitSeq: commit.CommitSeq, DurableSeq: append([]uint64(nil), seqs...)}
	return commit, receipt, false, nil
}

// Apply records a commit that was already validated for replay or a durable append.
func (c *Chain) Apply(commit Commit) error {
	return c.check(commit, true, model.CodeIncompatibleVersion)
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
		return model.NewError(code, "unknown record type")
	}
	if commit.Version != 1 {
		return model.NewError(code, "unknown commit version")
	}
	if commit.CommitID == "" {
		return model.NewError(code, "commit id is required")
	}
	if _, ok := c.commitIDs[commit.CommitID]; ok {
		return model.NewError(code, "duplicate commit id")
	}
	if commit.CommitSeq != c.last+1 || commit.ExpectedPreviousSeq != c.last {
		return model.NewError(code, "commit sequence does not continue")
	}
	var entryAdd []string
	for _, rec := range commit.Entries {
		if rec.Version != 1 || rec.Type == "" || rec.ID == "" {
			return model.NewError(code, "entry is missing type, version, or id")
		}
		if _, ok := c.entryIDs[rec.ID]; ok || containsID(entryAdd, rec.ID) {
			return model.NewError(code, "duplicate entry id")
		}
		if rec.ParentID != "" && !c.hasEntry(rec.ParentID) && !containsID(entryAdd, rec.ParentID) {
			return model.NewError(code, "entry parent is missing")
		}
		entryAdd = append(entryAdd, rec.ID)
	}
	for _, rec := range commit.ControlRecords {
		if rec.Version != 1 || rec.Type == "" || rec.ID == "" {
			return model.NewError(code, "control record is missing type, version, or id")
		}
	}
	cursor := c.cursor
	var eventAdd []string
	for _, ev := range commit.Events {
		if ev.SchemaVersion != 1 || ev.EventID == "" {
			return model.NewError(code, "event id or schema is invalid")
		}
		if _, ok := c.eventIDs[ev.EventID]; ok || containsID(eventAdd, ev.EventID) {
			return model.NewError(code, "duplicate event id")
		}
		if ev.DurableSeq == nil || *ev.DurableSeq != cursor+1 {
			return model.NewError(code, "durable sequence does not continue")
		}
		cursor = *ev.DurableSeq
		eventAdd = append(eventAdd, ev.EventID)
	}
	if commit.IdempotencyKey != "" {
		if _, ok := c.idem[commit.IdempotencyKey]; ok {
			return model.NewError(code, "duplicate idempotency key")
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

func CloneSession(in StoredSession) StoredSession {
	out := in
	out.Header = cloneHeader(in.Header)
	if in.Commits == nil {
		out.Commits = nil
		return out
	}
	out.Commits = make([]Commit, len(in.Commits))
	for i := range in.Commits {
		out.Commits[i] = CloneCommit(in.Commits[i])
	}
	return out
}

func CloneCommit(in Commit) Commit {
	out := in
	out.ContentDigest = cloneBytes(in.ContentDigest)
	out.Entries = cloneRecords(in.Entries)
	out.ControlRecords = cloneRecords(in.ControlRecords)
	if in.BranchUpdates == nil {
		out.BranchUpdates = nil
	} else {
		out.BranchUpdates = append([]BranchUpdate(nil), in.BranchUpdates...)
	}
	if in.Events == nil {
		out.Events = nil
		return out
	}
	out.Events = make([]agent.Event, len(in.Events))
	for i := range in.Events {
		ev := in.Events[i]
		ev.Payload = cloneRaw(ev.Payload)
		if ev.DurableSeq != nil {
			n := *ev.DurableSeq
			ev.DurableSeq = &n
		}
		if ev.ChunkSeq != nil {
			n := *ev.ChunkSeq
			ev.ChunkSeq = &n
		}
		out.Events[i] = ev
	}
	return out
}

func CloneReceipt(in CommitReceipt) CommitReceipt {
	if in.DurableSeq == nil {
		return in
	}
	in.DurableSeq = append([]uint64(nil), in.DurableSeq...)
	return in
}

func cloneHeader(in Header) Header {
	in.Workspace = cloneRaw(in.Workspace)
	return in
}

func cloneRecords(in []Record) []Record {
	if in == nil {
		return nil
	}
	out := make([]Record, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Payload = cloneRaw(in[i].Payload)
	}
	return out
}

func cloneRaw(in json.RawMessage) json.RawMessage {
	return cloneBytes(in)
}

func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
