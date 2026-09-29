package state_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// Load-only corruption fixture checks state validation independently of the
// storage chain checks, including syntactically valid malicious envelopes.
type hostCommandReplayStore struct {
	store.Store
	stored store.StoredSession
}

func (s hostCommandReplayStore) Load(context.Context, string) (store.StoredSession, error) {
	return s.stored, nil
}
func TestHostCommandFactRejectsMalformedReplay(t *testing.T) {
	for _, field := range []string{"id", "session", "branch", "sequence", "version", "parent", "mixed-control", "mixed-entry", "branch-update", "event", "event-scope", "event-id", "event-version", "event-time", "event-sequence", "commit-id", "commit-version", "commit-type", "duplicate", "later-message"} {
		t.Run(field, func(t *testing.T) {
			m, s := fixture(t)
			if err := hostWriter(t, m).SaveHostCommand(t.Context(), hostResult("shell"), false); err != nil {
				t.Fatal(err)
			}
			stored, err := s.Load(t.Context(), "session")
			if err != nil {
				t.Fatal(err)
			}
			c := &stored.Commits[0]
			r := &c.ControlRecords[0]
			var result state.HostCommandResult
			if err := json.Unmarshal(r.Payload, &result); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "id":
				r.ID = "other"
			case "session":
				result.Message.Scope.SessionID = "other"
			case "branch":
				result.BranchID = "other"
			case "sequence":
				result.CommitSeq++
			case "version":
				r.Version++
			case "parent":
				r.ParentID = "model-leaf"
			case "mixed-control":
				c.ControlRecords = append(c.ControlRecords, store.Record{Type: "budget", Version: 1, ID: "budget", Payload: json.RawMessage(`{}`)})
			case "mixed-entry":
				c.Entries = []store.Record{{Type: "message", Version: 1, ID: "shell", Payload: json.RawMessage(`{}`)}}
			case "branch-update":
				c.BranchUpdates = []store.BranchUpdate{{BranchID: "other", Active: true}}
			case "event":
				c.Events[0].Payload = json.RawMessage(`{}`)
			case "event-scope":
				c.Events[0].Scope.TraceID = "trace"
			case "event-id":
				c.Events[0].EventID = ""
			case "event-version":
				c.Events[0].SchemaVersion++
			case "event-time":
				c.Events[0].OccurredAt = time.Time{}
			case "event-sequence":
				c.Events[0].DurableSeq = nil
			case "commit-id":
				c.CommitID = ""
			case "commit-version":
				c.Version++
			case "commit-type":
				c.RecordType = "other"
			case "duplicate":
				next := *c
				next.CommitSeq = 2
				next.ExpectedPreviousSeq = 1
				second := result
				second.CommitSeq = 2
				raw, _ := json.Marshal(second)
				next.ControlRecords = []store.Record{{Type: "host_command_result", Version: 1, ID: "shell", Payload: raw}}
				stored.Commits = append(stored.Commits, next)
			case "later-message":
				raw, _ := json.Marshal(result.Message)
				stored.Commits = append(stored.Commits, store.Commit{CommitSeq: 2, ExpectedPreviousSeq: 1, Entries: []store.Record{{Type: "message", Version: 1, ID: "shell", Payload: raw}}})
			}
			// The slice may have grown, so write through its current first element.
			stored.Commits[0].ControlRecords[0].Payload, _ = json.Marshal(result)
			if _, err := state.NewManager(hostCommandReplayStore{Store: s, stored: stored}, "session"); err == nil {
				t.Fatal("malformed host result replay accepted")
			}
		})
	}
}
func TestHostCommandFactViewIsolation(t *testing.T) {
	m, _ := fixture(t)
	msg := hostResult("shell")
	if err := hostWriter(t, m).SaveHostCommand(t.Context(), msg, false); err != nil {
		t.Fatal(err)
	}
	msg.Command.Content[0] = '!'
	view := m.View()
	r := view.HostCommands["shell"]
	r.Message.Command.Result[0] = '!'
	view.HostCommands["shell"] = r
	actual := m.View().HostCommands["shell"]
	if !json.Valid(actual.Message.Command.Content) || !json.Valid(actual.Message.Command.Result) {
		t.Fatal("caller mutated committed result")
	}
	if err := m.AppendMessage(t.Context(), agent.AgentMessage{ID: "shell", Kind: agent.KindCommand, Status: agent.StatusComplete, Command: hostResult("shell").Command}); err == nil {
		t.Fatal("unmarked consumption duplicated host identity")
	}
}
