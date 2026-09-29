package state

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// HostCommandResult is an immutable completed host request, not an execution
// queue or a model message. Consumption is a separate atomic history commit.
// CommitSeq orders display independently of the model history leaf.
type HostCommandResult struct {
	Message            agent.AgentMessage `json:"message"`
	ExcludeFromContext bool               `json:"excludeFromContext"`
	BranchID           string             `json:"branchId"`
	CommitSeq          uint64             `json:"commitSeq"`
}

func validateHostCommandResult(result HostCommandResult, sessionID string) error {
	msg := result.Message
	if err := msg.Validate(); err != nil {
		return err
	}
	if msg.Kind != agent.KindCommand || msg.Status != agent.StatusComplete || msg.Scope != (agent.MessageScope{SessionID: sessionID}) || sessionID == "" || msg.Source != (agent.SourceRef{Kind: agent.SourceHuman, Description: "host shell"}) || msg.Command.Name != "shell" || result.BranchID == "" || result.CommitSeq == 0 {
		return product.NewError(product.CodeStateConflict, "invalid host command identity or scope")
	}
	if msg.Command.ExcludeFromContext != result.ExcludeFromContext {
		return product.NewError(product.CodeStateConflict, "host command context flag differs")
	}
	_, _, err := agent.DecodeHostShellCommand(*msg.Command)
	return err
}

// ValidateHostCommandCommit checks the complete control-only envelope. It is not
// a resume exception: callers must additionally match the persisted immutable
// result and the original checkpoint session/branch before allowing progress.
func ValidateHostCommandCommit(v View, c store.Commit, sessionID string) error {
	var r *store.Record
	for i := range c.ControlRecords {
		if c.ControlRecords[i].Type == "host_command_result" {
			r = &c.ControlRecords[i]
			break
		}
	}
	if r == nil {
		return nil
	}
	if c.RecordType != "commit" || c.Version != 1 || c.CommitID == "" || c.CommitSeq == 0 || c.ExpectedPreviousSeq != c.CommitSeq-1 || len(c.ControlRecords) != 1 || len(c.Entries) != 0 || len(c.BranchUpdates) != 0 || len(c.Events) != 1 || r.Version != 1 || r.ParentID != "" {
		return product.NewError(product.CodeIncompatibleVersion, "host command result must be an isolated control-only commit")
	}
	var result HostCommandResult
	if err := json.Unmarshal(r.Payload, &result); err != nil {
		return err
	}
	if err := validateHostCommandResult(result, sessionID); err != nil {
		return err
	}
	if r.ID != result.Message.ID || result.CommitSeq != c.CommitSeq || result.BranchID != v.BranchID {
		return product.NewError(product.CodeStateConflict, "host command journal binding differs")
	}
	ev := c.Events[0]
	var displayed agent.AgentMessage
	if ev.SchemaVersion != 1 || ev.EventID == "" || ev.OccurredAt.IsZero() || ev.DurableSeq == nil || *ev.DurableSeq == 0 || ev.Type != "message.finalized" || ev.Scope != (agent.EventScope{SessionID: sessionID}) || json.Unmarshal(ev.Payload, &displayed) != nil || !reflect.DeepEqual(displayed, agent.PublicMessage(result.Message)) {
		return product.NewError(product.CodeStateConflict, "host command display event differs from its result")
	}
	return nil
}

// SaveHostCommand saves execution facts only. It neither runs a command nor
// changes the model history, and has no permission to consume pending context.
func (m *Manager) SaveHostCommand(ctx context.Context, msg agent.AgentMessage, exclude bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.fault != nil {
		return m.fault
	}
	msg = clone(msg)
	if msg.Command != nil {
		msg.Command.ExcludeFromContext = exclude
	}
	if old, ok := m.view.HostCommands[msg.ID]; ok {
		if reflect.DeepEqual(old.Message, msg) && old.ExcludeFromContext == exclude {
			return nil
		}
		return product.NewError(product.CodeStateConflict, "host command result identity already exists")
	}
	result := HostCommandResult{Message: msg, ExcludeFromContext: exclude, BranchID: m.view.BranchID, CommitSeq: m.view.LastSeq + 1}
	if err := validateHostCommandResult(result, m.sessionID); err != nil {
		return err
	}
	_, err := m.commit(ctx, []store.Record{record("host_command_result", msg.ID, result)}, nil, []agent.Event{m.event("message.finalized", "", "", msg)})
	return err
}

func applyHostCommandResult(v *View, r store.Record) error {
	var result HostCommandResult
	if err := json.Unmarshal(r.Payload, &result); err != nil {
		return err
	}
	if _, ok := v.HostCommands[r.ID]; ok {
		return product.NewError(product.CodeStateConflict, "duplicate host command result")
	}
	for _, msg := range v.Messages {
		if msg.ID == r.ID {
			return product.NewError(product.CodeStateConflict, "host command identity collides with history")
		}
	}
	if v.HostCommands == nil {
		v.HostCommands = make(map[string]HostCommandResult)
	}
	v.HostCommands[r.ID] = result
	return nil
}
