package history_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ww1489/seasprak/agent"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/session/history"
)

type failingStore struct {
	history.SessionStore
	fail bool
}

func (s *failingStore) Append(ctx context.Context, id string, e history.ExpectedCommit, c history.Commit) (history.CommitReceipt, error) {
	if s.fail {
		return history.CommitReceipt{}, errors.New("injected sync failure")
	}
	return s.SessionStore.Append(ctx, id, e, c)
}
func fixture(t *testing.T) (*history.Manager, *failingStore) {
	t.Helper()
	s, err := memory.Open("session", history.Header{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &failingStore{SessionStore: s}
	m, err := history.NewManager(f, "session")
	if err != nil {
		t.Fatal(err)
	}
	return m, f
}
func accept(t *testing.T, m *history.Manager, key string, content string) agent.InputReceipt {
	t.Helper()
	r, err := m.Accept(context.Background(), agent.InputCommand{Kind: "prompt", IdempotencyKey: key, Content: json.RawMessage(content)}, agent.TargetAgent{Name: "main", Version: "v1", Generation: "g"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestFailedCommitLeavesViewUnchanged(t *testing.T) {
	m, s := fixture(t)
	r := accept(t, m, "k", `{"text":"hello"}`)
	if err := m.SetTraceState(context.Background(), r.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	before := m.View()
	s.fail = true
	if err := m.SetTraceState(context.Background(), r.TraceID, "completed", true); err == nil {
		t.Fatal("expected storage error")
	}
	after := m.View()
	if after.Traces[r.TraceID].State != before.Traces[r.TraceID].State || after.Traces[r.TraceID].Settled {
		t.Fatal("failed commit changed committed view")
	}
	if err := m.Consume(context.Background(), r.InputID); err == nil {
		t.Fatal("expected consume error")
	}
	if m.View().Inputs[r.InputID].State != "pending" {
		t.Fatal("failed consumption changed state")
	}
}
func TestIdempotencyPreservesJSONTypesAndPrincipal(t *testing.T) {
	m, _ := fixture(t)
	accept(t, m, "k", `{"a":1}`)
	_, err := m.Accept(context.Background(), agent.InputCommand{Kind: "prompt", IdempotencyKey: "k", Content: json.RawMessage(`[["a",1]]`)}, agent.TargetAgent{Name: "main", Generation: "g"})
	if err == nil {
		t.Fatal("object and array collided")
	}
	first := accept(t, m, "ordered", `{"b":2,"a":9007199254740993}`)
	same := accept(t, m, "ordered", `{"a":9007199254740993,"b":2}`)
	if first != same {
		t.Fatal("key order changed identity")
	}
	other, err := m.Accept(context.Background(), agent.InputCommand{Kind: "prompt", Principal: "other", IdempotencyKey: "ordered", Content: json.RawMessage(`{"a":0}`)}, agent.TargetAgent{Name: "main", Generation: "g"})
	if err != nil || other.InputID == first.InputID {
		t.Fatal("principal scope not isolated", err)
	}
}
func TestReplayReceiptAndSnapshots(t *testing.T) {
	m, s := fixture(t)
	r := accept(t, m, "persist", `{"text":"hello"}`)
	copy := m.View()
	copy.Traces[r.TraceID].State = "tampered"
	delete(copy.Inputs, r.InputID)
	if m.View().Traces[r.TraceID].State == "tampered" || m.View().Inputs[r.InputID] == nil {
		t.Fatal("snapshot aliases committed state")
	}
	reopened, err := history.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	again := accept(t, reopened, "persist", `{"text":"hello"}`)
	if again != r {
		t.Fatalf("receipt changed across replay: %+v -> %+v", r, again)
	}
}
func TestTerminalTraceCannotRevive(t *testing.T) {
	m, _ := fixture(t)
	r := accept(t, m, "", `{"text":"hi"}`)
	if err := m.SetTraceState(context.Background(), r.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTraceState(context.Background(), r.TraceID, "completed", true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTraceState(context.Background(), r.TraceID, "running", false); err == nil {
		t.Fatal("terminal trace revived")
	}
}
