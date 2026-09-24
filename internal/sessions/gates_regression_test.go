package sessions_test

import (
	"context"
	"encoding/json"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestRuntimeRejectedToolStillPairsWithAcceptedCall(t *testing.T) {
	var executed bool
	fake := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "invalid", Name: "add", Arguments: `{"n":"bad"}`}}}, testkit.Step{Text: "understood"})
	s := openSession(t, fake, func(context.Context, json.RawMessage) (string, error) { executed = true; return "", nil })
	defer closeSession(t, s)
	r := submit(t, s)
	waitState(t, s, r.TraceID, "completed")
	snap, _ := s.Snapshot(context.Background())
	if executed {
		t.Fatal("invalid arguments executed")
	}
	if len(snap.Calls) != 1 {
		t.Fatal("accepted tool call was lost")
	}
	for _, call := range snap.Calls {
		if call.Observation == nil || call.Observation.Status != "failed" || call.Observation.Executed {
			t.Fatalf("bad rejection: %+v", call)
		}
	}
}
func TestRuntimeToolBudgetFailureStopsAfterPersistedResult(t *testing.T) {
	fake := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "one", Name: "add", Arguments: `{"n":1}`}}}, testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "two", Name: "add", Arguments: `{"n":2}`}}}, testkit.Step{Text: "must not run"})
	s, e := sessions.CreateAgentSession(context.Background(), sessions.Options{Workspace: t.TempDir(), StateRoot: "memory", Profile: sessions.ProfileMemory, Model: fake, Limits: config.Limits{TraceToolCalls: 1}, Tools: []tools.Definition{{Name: "add", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}}})
	if e != nil {
		t.Fatal(e)
	}
	defer closeSession(t, s)
	r := submit(t, s)
	waitState(t, s, r.TraceID, "failed")
	if fake.Calls() != 2 {
		t.Fatalf("model continued after budget failure: %d", fake.Calls())
	}
	snap, _ := s.Snapshot(context.Background())
	if !strings.Contains(snap.Traces[r.TraceID].Error, product.CodeBudgetExhausted) {
		t.Fatalf("budget failure masked by another error: %s", snap.Traces[r.TraceID].Error)
	}
	for _, call := range snap.Calls {
		if call.Observation == nil {
			t.Fatal("budget failure lost tool result")
		}
		if call.Call.ProviderCallID == "two" && (call.Observation.Status != "failed" || call.Observation.Executed || call.Observation.SideEffect != "none") {
			t.Fatalf("budget rejection became an unknown execution: %+v", call.Observation)
		}
	}
}
func TestRuntimePinsToolDefinitionsAtSessionStart(t *testing.T) {
	defs := []tools.Definition{{Name: "add", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { return "original", nil }}}
	fake := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "pinned", Name: "add", Arguments: `{}`}}}, testkit.Step{Text: "done"})
	s, e := sessions.CreateAgentSession(context.Background(), sessions.Options{Workspace: t.TempDir(), StateRoot: "memory", Profile: sessions.ProfileMemory, Model: fake, Tools: defs})
	if e != nil {
		t.Fatal(e)
	}
	defer closeSession(t, s)
	defs[0].Run = func(context.Context, json.RawMessage) (string, error) { return "replaced", nil }
	defs[0].Version = "v2"
	r := submit(t, s)
	waitState(t, s, r.TraceID, "completed")
	snap, _ := s.Snapshot(context.Background())
	for _, call := range snap.Calls {
		if call.Observation.Content != "original" {
			t.Fatal("accepted generation used mutated caller definitions")
		}
	}
}

func TestRuntimeReadOnlyRejectsMutations(t *testing.T) {
	s, _ := runtimeSession(t, &controlledModel{})
	r := submit(t, s)
	waitState(t, s, r.TraceID, "completed")
	// The SDK's manifest-missing read-only path is covered separately; this checks the write gate itself.
	// A readonly session is opened through the public SDK from an existing persisted journal.
	root, ws := t.TempDir(), t.TempDir()
	opts := sessions.Options{SessionID: "read-only", Workspace: ws, StateRoot: root, Profile: sessions.ProfileMemory, Model: testkit.NewFake()}
	writer, e := sessions.CreateAgentSession(context.Background(), opts)
	if e != nil {
		t.Fatal(e)
	}
	closeSession(t, writer)
	opts.ReadOnly = true
	opts.Model = nil
	reader, e := sessions.OpenAgentSession(context.Background(), opts)
	if e != nil {
		t.Fatal(e)
	}
	defer closeSession(t, reader)
	_, e = reader.SubmitInput(context.Background(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{}`)})
	if e == nil {
		t.Fatal("readonly accepted input")
	}
	if e = reader.Cancel(context.Background(), "anything"); e == nil {
		t.Fatal("readonly accepted cancel")
	}
	if e = reader.ContinueQueue(context.Background(), "anything"); e == nil {
		t.Fatal("readonly accepted continue")
	}
}
func TestRuntimeContinueQueueDoesNotOverlap(t *testing.T) {
	m := &controlledModel{entered: make(chan context.Context, 8), gate: make(chan struct{})}
	s, _ := runtimeSession(t, m)
	defer close(m.gate)
	first := submit(t, s)
	awaitStart(t, m)
	second := submit(t, s)
	if e := s.ContinueQueue(context.Background(), second.TraceID); e != nil {
		t.Fatal(e)
	}
	select {
	case <-m.entered:
		t.Fatal("ContinueQueue overlapped active execution")
	case <-time.After(50 * time.Millisecond):
	}
	if e := s.Cancel(context.Background(), first.TraceID); e != nil {
		t.Fatal(e)
	}
	if e := s.ContinueQueue(context.Background(), second.TraceID); e != nil {
		t.Fatal(e)
	}
	awaitStart(t, m)
}
