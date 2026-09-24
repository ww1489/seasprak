package session_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak"
	"github.com/ww1489/seasprak/agent"
	"github.com/ww1489/seasprak/agent/tools"
	"github.com/ww1489/seasprak/internal/testkit"
	"github.com/ww1489/seasprak/session"
)

func TestFakeModelLoopSettlesOnce(t *testing.T) {
	fake := testkit.NewFake(testkit.Step{Text: "done"})
	s := openSession(t, fake, nil)
	defer closeSession(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	receipt, err := s.SubmitInput(ctx, agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`), IdempotencyKey: "once"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.SubmitInput(ctx, agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`), IdempotencyKey: "once"})
	if err != nil {
		t.Fatal(err)
	}
	if again.TraceID != receipt.TraceID || again.InputID != receipt.InputID {
		t.Fatalf("idempotent retry changed identity: %+v %+v", receipt, again)
	}
	waitState(t, s, receipt.TraceID, "completed")
	if fake.Calls() == 0 {
		t.Fatal("model was not called")
	}
}

func TestMissingWorkspace(t *testing.T) {
	_, err := seasprak.CreateAgentSession(context.Background(), session.Options{Profile: session.ProfileMemory})
	if err == nil {
		t.Fatal("missing workspace must be rejected")
	}
}

func TestReopenDoesNotRun(t *testing.T) {
	fake := testkit.NewFake(testkit.Step{Text: "done"})
	root := t.TempDir()
	ws := t.TempDir()
	opts := session.Options{
		Workspace: ws, StateRoot: root, SessionID: "sess-reopen", Profile: session.ProfileMemory, Model: fake,
		Instruction: "memory tools only",
		Tools: []tools.Definition{{
			Name: "add", Version: "v1", Description: "add a number", Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
			Run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
		}},
		ToolInfos: []*schema.ToolInfo{seasprak.ToolInfo("add", "add a number")},
	}
	s, err := seasprak.CreateAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	receipt, err := s.SubmitInput(ctx, agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, s, receipt.TraceID, "completed")
	closeSession(t, s)
	reopenedFake := testkit.NewFake(testkit.Step{Text: "should-not-run"})
	opts.Model = reopenedFake
	reopened, err := seasprak.OpenAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, reopened)
	time.Sleep(100 * time.Millisecond)
	if reopenedFake.Calls() != 0 {
		t.Fatal("reopen executed the model")
	}
}

func TestDefaultProfileRejectsMissingBackends(t *testing.T) {
	_, err := seasprak.CreateAgentSession(context.Background(), session.Options{Workspace: t.TempDir(), Profile: session.ProfileDefault})
	if err == nil {
		t.Fatal("default profile must fail closed")
	}
}

func openSession(t *testing.T, fake *testkit.FakeModel, run func(context.Context, json.RawMessage) (string, error)) *session.AgentSession {
	t.Helper()
	if run == nil {
		run = func(context.Context, json.RawMessage) (string, error) { return "ok", nil }
	}
	_ = run
	s, err := seasprak.CreateAgentSession(context.Background(), session.Options{
		Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: session.ProfileMemory, Model: fake,
		Instruction: "memory tools only",
		Tools: []tools.Definition{{
			Name: "add", Version: "v1", Description: "add a number", Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`), Run: run,
		}},
		ToolInfos: []*schema.ToolInfo{seasprak.ToolInfo("add", "add a number")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func closeSession(t *testing.T, s *session.AgentSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func waitState(t *testing.T, s *session.AgentSession, traceID, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := s.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if trace := snap.Traces[traceID]; trace != nil && trace.Settled {
			if trace.State != want {
				t.Fatalf("trace settled as %s, want %s; error=%s", trace.State, want, trace.Error)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	snap, _ := s.Snapshot(context.Background())
	t.Fatalf("trace %s did not reach %s; snapshot=%+v", traceID, want, snap.Traces[traceID])
}
