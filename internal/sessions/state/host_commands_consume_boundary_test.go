package state_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
)

func TestHostCommandConsumptionPreservesNonterminalTrace(t *testing.T) {
	for _, status := range []string{"running", "paused", "cancelling"} {
		t.Run(status, func(t *testing.T) {
			m, _ := fixture(t)
			in, err := m.Accept(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"original"}`)}, agent.TargetAgent{Name: "main", Generation: "gen"})
			if err != nil {
				t.Fatal(err)
			}
			if err := m.SetTraceState(t.Context(), in.TraceID, "running", false); err != nil {
				t.Fatal(err)
			}
			if err := m.SetTraceState(t.Context(), in.TraceID, status, false); err != nil {
				t.Fatal(err)
			}
			if err := m.SaveHostCommand(t.Context(), hostResult("pending"), false); err != nil {
				t.Fatal(err)
			}
			before := m.View()
			if err := m.ConsumeHostCommands(t.Context()); err != nil || !reflect.DeepEqual(before, m.View()) {
				t.Fatal("nonterminal trace consumed command", err)
			}
			if err := m.SetTraceState(t.Context(), in.TraceID, "cancelling", false); err != nil {
				t.Fatal(err)
			}
			if err := m.SetTraceState(t.Context(), in.TraceID, "cancelled", true); err != nil {
				t.Fatal(err)
			}
			if err := m.ConsumeHostCommands(t.Context()); err != nil || len(m.View().Messages) != 1 {
				t.Fatal("terminal trace did not release context", err)
			}
		})
	}
}

func TestHostCommandConsumptionDiskFailure(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "lost-ack"}[lost], func(t *testing.T) {
			root := t.TempDir()
			backend, err := jsonl.Open("session", root, store.Header{}, jsonl.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = backend.Close() })
			faults := &hostResultFaultStore{Store: backend, lost: lost, phase: "host_command_consumed"}
			m, err := state.NewManager(faults, "session")
			if err != nil {
				t.Fatal(err)
			}
			if err := m.SaveHostCommand(t.Context(), hostResult("pending"), false); err != nil {
				t.Fatal(err)
			}
			before := m.View()
			requireP2Code(t, m.ConsumeHostCommands(t.Context()), product.CodeStorageUnavailable)
			if !reflect.DeepEqual(before, m.View()) || m.Fault() == nil {
				t.Fatal("failed consumption exposed candidate")
			}
			if err := backend.Close(); err != nil {
				t.Fatal(err)
			}
			disk, err := jsonl.Open("session", root, store.Header{}, jsonl.Options{OpenExisting: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = disk.Close() })
			reopened, err := state.NewManager(disk, "session")
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if lost {
				want = 1
			}
			view := reopened.View()
			if len(view.Messages) != want || len(view.HostCommandConsumptions) != want || !reflect.DeepEqual(view.HostCommands, before.HostCommands) {
				t.Fatal("reopen lost result or exposed partial consumption")
			}
			if err := reopened.ConsumeHostCommands(t.Context()); err != nil {
				t.Fatal(err)
			}
			after := reopened.View()
			if len(after.Messages) != 1 || len(after.HostCommandConsumptions) != 1 || after.Cursor != before.Cursor {
				t.Fatal("recovery duplicated history or display event")
			}
			if err := reopened.ConsumeHostCommands(t.Context()); err != nil || !reflect.DeepEqual(after, reopened.View()) {
				t.Fatal("recovery duplicated consumption", err)
			}
		})
	}
}

func TestHostCommandConsumptionRejectsMalformedReplay(t *testing.T) {
	for _, field := range []string{"id", "session", "branch", "result-sequence", "sequence", "version", "parent", "order", "missing-entry", "extra-entry", "changed-result", "event", "branch-update", "duplicate"} {
		t.Run(field, func(t *testing.T) {
			m, s := fixture(t)
			for _, id := range []string{"first", "second"} {
				if err := m.SaveHostCommand(t.Context(), hostResult(id), false); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.ConsumeHostCommands(t.Context()); err != nil {
				t.Fatal(err)
			}
			stored, err := s.Load(t.Context(), "session")
			if err != nil {
				t.Fatal(err)
			}
			c := &stored.Commits[len(stored.Commits)-1]
			var binding state.HostCommandConsumption
			if err := json.Unmarshal(c.ControlRecords[0].Payload, &binding); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "id":
				binding.CommandID = "other"
			case "session":
				binding.SessionID = "other"
			case "branch":
				binding.BranchID = "other"
			case "result-sequence":
				binding.ResultCommitSeq++
			case "sequence":
				binding.CommitSeq++
			case "version":
				c.ControlRecords[0].Version++
			case "parent":
				c.ControlRecords[0].ParentID = "leaf"
			case "order":
				c.ControlRecords[0], c.ControlRecords[1] = c.ControlRecords[1], c.ControlRecords[0]
			case "missing-entry":
				c.Entries = c.Entries[:1]
			case "extra-entry":
				c.Entries = append(c.Entries, c.Entries[0])
			case "changed-result":
				c.Entries[0].Payload = json.RawMessage(`{}`)
			case "event":
				c.Events = []agent.Event{{Type: "message.finalized"}}
			case "branch-update":
				c.BranchUpdates = []store.BranchUpdate{{BranchID: "other"}}
			case "duplicate":
				stored.Commits = append(stored.Commits, *c)
			}
			if field != "order" {
				stored.Commits[2].ControlRecords[0].Payload, _ = json.Marshal(binding)
			}
			if _, err := state.NewManager(hostCommandReplayStore{Store: s, stored: stored}, "session"); err == nil {
				t.Fatal("invalid consumption accepted")
			}
		})
	}
}
