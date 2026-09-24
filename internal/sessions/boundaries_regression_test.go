package sessions_test

import (
	"context"
	"encoding/json"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestRuntimeRetryUsesSameLogicalTurn(t *testing.T) {
	fake := testkit.NewFake(testkit.Step{Err: &product.Error{Code: product.CodeResourceUnavailable, Message: "temporary", Retryable: true}}, testkit.Step{Text: "success"})
	s, e := sessions.CreateAgentSession(context.Background(), sessions.Options{Workspace: t.TempDir(), StateRoot: "memory", Profile: sessions.ProfileMemory, Model: fake, Limits: config.Limits{TraceLogicalModelCalls: 1, TraceTransportRequests: 3}})
	if e != nil {
		t.Fatal(e)
	}
	defer closeSession(t, s)
	r := submit(t, s)
	waitState(t, s, r.TraceID, "completed")
	snap, _ := s.Snapshot(context.Background())
	usage := snap.Traces[r.TraceID].Usage
	if usage.LogicalModelCalls != 1 || usage.TransportRequests != 2 || len(snap.Turns) != 1 {
		t.Fatalf("retry changed logical turn: %+v turns=%d", usage, len(snap.Turns))
	}
}
func TestRuntimeConsumesConsecutiveSteeringAtSeparateBoundaries(t *testing.T) {
	gate := make(chan struct{})
	fake := testkit.NewFake(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "first", Name: "add", Arguments: `{}`}}}, testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "second", Name: "add", Arguments: `{}`}}}, testkit.Step{Text: "done"})
	capture := &capturingModel{inner: fake}
	s, e := sessions.CreateAgentSession(context.Background(), sessions.Options{Workspace: t.TempDir(), StateRoot: "memory", Profile: sessions.ProfileMemory, Model: capture, Tools: []tools.Definition{{Name: "add", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}}})
	if e != nil {
		t.Fatal(e)
	}
	defer closeSession(t, s)
	r := submit(t, s)
	deadline := time.Now().Add(3 * time.Second)
	for fake.Calls() == 0 {
		if time.Now().After(deadline) {
			close(gate)
			t.Fatal("first model never started")
		}
		time.Sleep(time.Millisecond)
	}
	a, e := s.SubmitInput(context.Background(), agent.InputCommand{Kind: "steering", TargetTraceID: r.TraceID, Content: json.RawMessage(`{"text":"first steering"}`)})
	if e != nil {
		close(gate)
		t.Fatal(e)
	}
	b, e := s.SubmitInput(context.Background(), agent.InputCommand{Kind: "steering", TargetTraceID: r.TraceID, Content: json.RawMessage(`{"text":"second steering"}`)})
	if e != nil {
		close(gate)
		t.Fatal(e)
	}
	close(gate)
	waitState(t, s, r.TraceID, "completed")
	snap, _ := s.Snapshot(context.Background())
	if snap.Inputs[a.InputID].State != "consumed" || snap.Inputs[b.InputID].State != "consumed" {
		t.Fatal("steering was blocked by a consumed queue head")
	}
	requests := capture.requests()
	if len(requests) != 3 {
		t.Fatalf("requests=%d", len(requests))
	}
	last1 := requests[1][len(requests[1])-1]
	last2 := requests[2][len(requests[2])-1]
	if last1.ContentBlocks[0].UserInputText.Text != "first steering" || last2.ContentBlocks[0].UserInputText.Text != "second steering" {
		t.Fatal("steering did not cross separate logical boundaries")
	}
}
func TestRuntimeCloseTimeoutRetainsWriterLock(t *testing.T) {
	m := &controlledModel{entered: make(chan context.Context, 8), gate: make(chan struct{}), ignoreCancel: true}
	opts := sessions.Options{SessionID: "locked-close", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sessions.ProfileMemory, Model: m}
	s, e := sessions.CreateAgentSession(context.Background(), opts)
	if e != nil {
		t.Fatal(e)
	}
	r := submit(t, s)
	awaitStart(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if e = s.Close(ctx); e == nil {
		close(m.gate)
		t.Fatal("premature close")
	}
	opts.Model = testkit.NewFake()
	opened, e := sessions.OpenAgentSession(context.Background(), opts)
	if e == nil {
		closeSession(t, opened)
		close(m.gate)
		t.Fatal("writer lock released before execution stopped")
	}
	close(m.gate)
	closeSession(t, s)
	opened, e = sessions.OpenAgentSession(context.Background(), opts)
	if e != nil {
		t.Fatal(e)
	}
	defer closeSession(t, opened)
	snap, _ := opened.Snapshot(context.Background())
	if snap.Traces[r.TraceID].State != "paused" || snap.Traces[r.TraceID].Settled {
		t.Fatal("close fabricated terminal completion")
	}
}
