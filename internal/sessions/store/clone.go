package store

import (
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
)

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
