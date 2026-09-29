package state_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func TestToolOutputProjectionPreservesFactsAndReplays(t *testing.T) {
	_, s := fixture(t)
	original := agent.ToolObservation{Status: "failed", SideEffect: "confirmed", Executed: true, Process: true, ExitCode: 23, Terminated: true, Content: "safe preview", Truncated: true, LogError: "log unavailable"}
	seedP2Call(t, s, &original)
	m, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	p := agent.ToolOutputProjection{CallID: "call", Observation: original, Content: "head\ntail", Truncated: true, Artifact: agent.ArtifactRef{ID: "log", SessionID: "session", Environment: "memory", Hash: "digest", Size: 123, Available: true}}
	if err := m.SaveToolProjection(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	after := m.View()
	if *after.Calls["call"].Observation != original || after.ToolProjections["call"] != p {
		t.Fatal("projection changed execution facts")
	}
	if err := m.SaveToolProjection(t.Context(), p); err != nil || m.View().LastSeq != after.LastSeq {
		t.Fatal("identical projection was not idempotent", err)
	}
	reopened, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.View().ToolProjections["call"] != p || *reopened.View().Calls["call"].Observation != original {
		t.Fatal("projection or original observation lost on replay")
	}
	for _, field := range []string{"status", "exit", "call", "session", "content"} {
		changed := p
		switch field {
		case "status":
			changed.Observation.Status = "succeeded"
		case "exit":
			changed.Observation.ExitCode = 0
		case "call":
			changed.CallID = "other"
		case "session":
			changed.Artifact.SessionID = "other"
		case "content":
			changed.Content = "replacement"
		}
		requireP2Code(t, m.SaveToolProjection(context.Background(), changed), product.CodeStateConflict)
		if !reflect.DeepEqual(after, m.View()) {
			t.Fatal("rejected projection changed state")
		}
	}
}

func TestToolOutputProjectionRequiresCommittedObservation(t *testing.T) {
	_, s := fixture(t)
	seedP2Call(t, s, nil)
	m, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	before := m.View()
	requireP2Code(t, m.SaveToolProjection(t.Context(), agent.ToolOutputProjection{CallID: "call", Observation: agent.ToolObservation{Status: "succeeded"}, Content: "uncommitted"}), product.CodeStateConflict)
	if !reflect.DeepEqual(before, m.View()) {
		t.Fatal("unobserved projection changed state")
	}
}

func TestToolOutputProjectionFailedCommitInvisible(t *testing.T) {
	_, s := fixture(t)
	original := agent.ToolObservation{Status: "succeeded", Process: true, Terminated: true}
	seedP2Call(t, s, &original)
	m, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	before := m.View()
	s.fail = true
	if err := m.SaveToolProjection(t.Context(), agent.ToolOutputProjection{CallID: "call", Observation: original, Content: "preview"}); err == nil || !reflect.DeepEqual(before, m.View()) {
		t.Fatal("failed projection changed state", err)
	}
}
