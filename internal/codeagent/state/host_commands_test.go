package state_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

type hostCommandWriter interface {
	SaveHostCommand(context.Context, agent.AgentMessage, bool) error
}

func hostResult(id string) agent.AgentMessage {
	return agent.AgentMessage{ID: id, Kind: agent.KindCommand, Status: agent.StatusComplete, Scope: agent.MessageScope{SessionID: "session"}, Source: agent.SourceRef{Kind: agent.SourceHuman, Description: "host shell"}, Command: &agent.CommandMessage{Name: "shell", Content: json.RawMessage(`{"command":"echo fixture"}`), Result: json.RawMessage(`{"output":"fixture","started":true,"terminated":true,"exitCode":0}`)}}
}
func hostWriter(t *testing.T, m *state.Manager) hostCommandWriter {
	t.Helper()
	w, ok := any(m).(hostCommandWriter)
	if !ok {
		t.Fatal("durable host-command fact writer is not implemented")
	}
	return w
}
func TestHostCommandFactPersistsWithoutModelHistory(t *testing.T) {
	for _, exclude := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "excluded"}[exclude], func(t *testing.T) {
			m, s := fixture(t)
			w := hostWriter(t, m)
			before := m.View()
			msg := hostResult("shell-one")
			if err := w.SaveHostCommand(t.Context(), msg, exclude); err != nil {
				t.Fatal(err)
			}
			after := m.View()
			if after.LeafID != before.LeafID || !reflect.DeepEqual(after.Messages, before.Messages) || len(after.Calls) != 0 || after.LastSeq != before.LastSeq+1 {
				t.Fatal("host fact changed model history or execution")
			}
			stored, err := s.Load(t.Context(), "session")
			if err != nil {
				t.Fatal(err)
			}
			c := stored.Commits[len(stored.Commits)-1]
			if len(c.Entries) != 0 || len(c.ControlRecords) != 1 || c.ControlRecords[0].Type != "host_command_result" || len(c.Events) != 1 || c.Events[0].Type != "message.finalized" {
				t.Fatal("host result is not one atomic control-only fact")
			}
			if err := w.SaveHostCommand(t.Context(), msg, exclude); err != nil || m.View().LastSeq != after.LastSeq {
				t.Fatal("same result was written twice", err)
			}
			msg.Command.Result = json.RawMessage(`{"output":"replacement"}`)
			requireP2Code(t, w.SaveHostCommand(t.Context(), msg, exclude), product.CodeStateConflict)
			requireP2Code(t, w.SaveHostCommand(t.Context(), hostResult("shell-one"), !exclude), product.CodeStateConflict)
			reopened, err := state.NewManager(s, "session")
			if err != nil || !reflect.DeepEqual(reopened.View(), after) {
				t.Fatal("result lost on replay", err)
			}
			// Replaying twice must remain a pure read.
			_, err = state.NewManager(s, "session")
			if err != nil {
				t.Fatal(err)
			}
			reloaded, _ := s.Load(t.Context(), "session")
			if len(reloaded.Commits) != len(stored.Commits) {
				t.Fatal("replay wrote to journal")
			}
		})
	}
}
func TestHostCommandFactRejectsIdentityAndScope(t *testing.T) {
	for _, field := range []string{"session", "trace", "turn", "tool", "source", "kind", "status", "request", "result", "duplicate-message"} {
		t.Run(field, func(t *testing.T) {
			m, _ := fixture(t)
			w := hostWriter(t, m)
			msg := hostResult("shell")
			switch field {
			case "session":
				msg.Scope.SessionID = "other"
			case "trace":
				msg.Scope.TraceID = "trace"
			case "turn":
				msg.Scope.TurnID = "turn"
			case "tool":
				msg.Scope.ToolCallID = "call"
			case "source":
				msg.Source.Kind = agent.SourceModel
			case "kind":
				msg.Kind = agent.KindUser
			case "status":
				msg.Status = agent.StatusIncomplete
			case "request":
				msg.Command.Content = json.RawMessage(`[]`)
			case "result":
				msg.Command.Result = nil
			case "duplicate-message":
				if err := m.AppendMessage(t.Context(), msg); err != nil {
					t.Fatal(err)
				}
			}
			before := m.View()
			if err := w.SaveHostCommand(t.Context(), msg, false); err == nil {
				t.Fatal("invalid host fact accepted")
			}
			if !reflect.DeepEqual(before, m.View()) {
				t.Fatal("rejection changed view")
			}
		})
	}
}

type hostResultFaultStore struct {
	store.Store
	phase string
	lost  bool
}

func (s *hostResultFaultStore) Append(ctx context.Context, id string, e store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	phase := s.phase
	if phase == "" {
		phase = "host_command_result"
	}
	for _, r := range c.ControlRecords {
		if r.Type == phase {
			if s.lost {
				if _, err := s.Store.Append(ctx, id, e, c); err != nil {
					return store.CommitReceipt{}, err
				}
			}
			return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "injected host result failure")
		}
	}
	return s.Store.Append(ctx, id, e, c)
}
func TestHostCommandFactStorageFailure(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "lost-ack"}[lost], func(t *testing.T) {
			_, base := fixture(t)
			backend := &hostResultFaultStore{Store: base, lost: lost}
			m, err := state.NewManager(backend, "session")
			if err != nil {
				t.Fatal(err)
			}
			w := hostWriter(t, m)
			before := m.View()
			requireP2Code(t, w.SaveHostCommand(t.Context(), hostResult("shell"), false), product.CodeStorageUnavailable)
			if !reflect.DeepEqual(before, m.View()) || m.Fault() == nil {
				t.Fatal("failed result was published or manager not faulted")
			}
			reopened, err := state.NewManager(base, "session")
			if err != nil {
				t.Fatal(err)
			}
			stored, _ := base.Load(t.Context(), "session")
			want := 0
			if lost {
				want = 1
			}
			if len(stored.Commits) != want || reopened.View().LeafID != "" || len(reopened.View().Messages) != 0 {
				t.Fatal("replay invented/lost result or model history")
			}
			if lost {
				if err := hostWriter(t, reopened).SaveHostCommand(t.Context(), hostResult("shell"), false); err != nil || reopened.View().LastSeq != 1 {
					t.Fatal("lost-ack recovery duplicated fact", err)
				}
			}
		})
	}
}
