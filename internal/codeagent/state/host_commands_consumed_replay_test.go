package state_test

import (
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/codeagent/state"
	store "github.com/ww1489/seasprak/internal/storage"
)

func TestHostCommandConsumedEntryCannotReplayWithoutMarker(t *testing.T) {
	m, s := fixture(t)
	if err := m.SaveHostCommand(t.Context(), hostResult("shell"), false); err != nil {
		t.Fatal(err)
	}
	if err := m.ConsumeHostCommands(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Load(t.Context(), "session")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(m.View().HostCommands["shell"].Message)
	stored.Commits = append(stored.Commits, store.Commit{CommitSeq: m.View().LastSeq, Entries: []store.Record{{Type: "message", Version: 1, ID: "shell", ParentID: "shell", Payload: payload}}})
	if _, err := state.NewManager(hostCommandReplayStore{Store: s, stored: stored}, "session"); err == nil {
		t.Fatal("unmarked entry reused an old consumption sequence")
	}
}
