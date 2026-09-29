package state

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// HostCommandConsumption binds an atomic history append to its immutable result.
// Excluded commands also receive a marker, but never a model-history entry.
type HostCommandConsumption struct {
	SessionID       string `json:"sessionId"`
	BranchID        string `json:"branchId"`
	CommandID       string `json:"commandId"`
	ResultCommitSeq uint64 `json:"resultCommitSeq"`
	CommitSeq       uint64 `json:"commitSeq"`
}

func hostCommandContextBlocked(v View) bool {
	for _, tr := range v.Traces {
		if !terminal(tr.State) && (tr.Started || tr.State != "queued") {
			return true
		}
	}
	return false
}
func pendingHostCommands(v View) []HostCommandResult {
	var pending []HostCommandResult
	for id, r := range v.HostCommands {
		if r.BranchID == v.BranchID && v.HostCommandConsumptions[id] == 0 {
			pending = append(pending, r)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].CommitSeq < pending[j].CommitSeq })
	return pending
}

// ConsumeHostCommands is a no-op while any started/nonqueued Trace is not
// terminal, including ordinary pauses and approval waits. The mailbox also
// checks its live execution before calling this method. Open never calls it.
func (m *Manager) ConsumeHostCommands(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.fault != nil {
		return m.fault
	}
	if hostCommandContextBlocked(*m.view) {
		return nil
	}
	pending := pendingHostCommands(*m.view)
	if len(pending) == 0 {
		return nil
	}
	controls := make([]store.Record, 0, len(pending))
	var entries []store.Record
	parent := m.view.LeafID
	for _, result := range pending {
		msg := result.Message
		consumed := HostCommandConsumption{SessionID: m.sessionID, BranchID: m.view.BranchID, CommandID: msg.ID, ResultCommitSeq: result.CommitSeq, CommitSeq: m.view.LastSeq + 1}
		controls = append(controls, record("host_command_consumed", msg.ID, consumed))
		if !result.ExcludeFromContext {
			entry := record("message", msg.ID, msg)
			entry.ParentID = parent
			parent = msg.ID
			entries = append(entries, entry)
		}
	}
	_, err := m.commit(ctx, controls, entries, nil)
	return err
}

func validateHostCommandConsumptionCommit(v View, c store.Commit, sessionID string) error {
	found := false
	for _, r := range c.ControlRecords {
		if r.Type == "host_command_consumed" {
			found = true
		}
	}
	if !found {
		return nil
	}
	pending := pendingHostCommands(v)
	if hostCommandContextBlocked(v) || len(c.Events) != 0 || len(c.BranchUpdates) != 0 || len(c.ControlRecords) != len(pending) || len(pending) == 0 {
		return product.NewError(product.CodeStateConflict, "host command consumption is not an isolated safe-boundary commit")
	}
	nextEntry := 0
	parent := v.LeafID
	for i, r := range c.ControlRecords {
		result := pending[i]
		var consumed HostCommandConsumption
		expected := HostCommandConsumption{SessionID: sessionID, BranchID: v.BranchID, CommandID: result.Message.ID, ResultCommitSeq: result.CommitSeq, CommitSeq: c.CommitSeq}
		if r.Type != "host_command_consumed" || r.Version != 1 || r.ParentID != "" || r.ID != result.Message.ID || json.Unmarshal(r.Payload, &consumed) != nil || consumed != expected {
			return product.NewError(product.CodeStateConflict, "host command consumption binding or order differs")
		}
		if result.ExcludeFromContext {
			continue
		}
		if nextEntry >= len(c.Entries) {
			return product.NewError(product.CodeStateConflict, "host command consumption lost its history entry")
		}
		e := c.Entries[nextEntry]
		nextEntry++
		var got agent.AgentMessage
		if json.Unmarshal(e.Payload, &got) != nil || !reflect.DeepEqual(got, result.Message) || e.ID != result.Message.ID || e.Type != "message" || e.Version != 1 || e.ParentID != parent {
			return product.NewError(product.CodeStateConflict, "host command consumption changed the original result")
		}
		parent = e.ID
	}
	if nextEntry != len(c.Entries) {
		return product.NewError(product.CodeStateConflict, "host command consumption contains unrelated history")
	}
	return nil
}

func applyHostCommandConsumption(v *View, r store.Record) error {
	var consumed HostCommandConsumption
	if err := json.Unmarshal(r.Payload, &consumed); err != nil {
		return err
	}
	if v.HostCommandConsumptions == nil {
		v.HostCommandConsumptions = make(map[string]uint64)
	}
	v.HostCommandConsumptions[r.ID] = consumed.CommitSeq
	return nil
}
