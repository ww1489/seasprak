package sessions_test

import (
	"context"
	"encoding/json"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type capturingModel struct {
	inner  *testkit.FakeModel
	mu     sync.Mutex
	inputs [][]*schema.AgenticMessage
}

func (m *capturingModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.AgenticMessage, error) {
	raw, _ := json.Marshal(in)
	var copy []*schema.AgenticMessage
	_ = json.Unmarshal(raw, &copy)
	m.mu.Lock()
	m.inputs = append(m.inputs, copy)
	m.mu.Unlock()
	return m.inner.Generate(ctx, in, opts...)
}
func (m *capturingModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, e := m.Generate(ctx, in, opts...)
	if e != nil {
		return nil, e
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}
func (m *capturingModel) requests() [][]*schema.AgenticMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]*schema.AgenticMessage(nil), m.inputs...)
}
func TestRuntimePersistsOrderedToolResultsAndTurns(t *testing.T) {
	m := &capturingModel{inner: testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider-one", Name: "add", Arguments: `{"n":1}`}, {CallID: "provider-two", Name: "add", Arguments: `{"n":2}`}}}, testkit.Step{Text: "finished"})}
	store, _ := memory.Open("tools", store.Header{})
	manager, _ := state.NewManager(store, "tools")
	secondStarted := make(chan struct{})
	var executions atomic.Int32
	s, e := sessions.Start(sessions.Options{SessionID: "tools", Profile: sessions.ProfileMemory, Store: store, Model: m, Tools: []tools.Definition{{Name: "add", Version: "v1", Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`), Execution: tools.ExecutionDescription{Effect: "read", Resources: []agent.ExecutionResource{{Identity: "ordered-results"}}}, Run: func(ctx context.Context, args json.RawMessage) (string, error) {
		executions.Add(1)
		var p struct {
			N int `json:"n"`
		}
		_ = json.Unmarshal(args, &p)
		if p.N == 2 {
			close(secondStarted)
		} else {
			select {
			case <-secondStarted:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return string(args), nil
	}}}, ToolInfos: []*schema.ToolInfo{testkit.ToolInfo("add", "")}}, manager, "gen")
	if e != nil {
		t.Fatal(e)
	}
	defer closeSession(t, s)
	r := submit(t, s)
	waitState(t, s, r.TraceID, "completed")
	view := manager.View()
	if executions.Load() != 2 {
		t.Fatalf("executions=%d", executions.Load())
	}
	if len(view.Turns) != 2 || len(view.Calls) != 2 {
		t.Fatalf("turns=%d calls=%d", len(view.Turns), len(view.Calls))
	}
	for _, turn := range view.Turns {
		if !turn.Ended {
			t.Errorf("turn not ended: %+v", turn)
		}
	}
	var ids []string
	for _, msg := range view.Messages {
		if msg.Kind == agent.KindToolResult {
			result := msg.Standard.ContentBlocks[0].FunctionToolResult
			ids = append(ids, result.CallID)
			if msg.Scope.ToolCallID == "" || msg.Scope.TurnID == "" {
				t.Error("result lost execution scope")
			}
		}
	}
	if len(ids) != 2 || ids[0] != "provider-one" || ids[1] != "provider-two" {
		t.Fatalf("result order = %v", ids)
	}
	replay, e := state.NewManager(store, "tools")
	if e != nil {
		t.Fatal(e)
	}
	if len(replay.View().Messages) != len(view.Messages) {
		t.Fatal("replay dropped history")
	}
	requests := m.requests()
	if len(requests) != 2 {
		t.Fatalf("model requests=%d", len(requests))
	}
	if len(requests[1]) < 4 {
		t.Fatal("next model request is missing the tool result group")
	}
}
func TestRuntimeUsesCompleteHistoryAndPerTraceBudget(t *testing.T) {
	m := &capturingModel{inner: testkit.NewFake(testkit.Step{Text: "first answer"}, testkit.Step{Text: "second answer"})}
	store, _ := memory.Open("history", store.Header{})
	manager, _ := state.NewManager(store, "history")
	s, e := sessions.Start(sessions.Options{SessionID: "history", Profile: sessions.ProfileMemory, Store: store, Model: m, Limits: config.Limits{TraceLogicalModelCalls: 1, TraceTransportRequests: 1}}, manager, "gen")
	if e != nil {
		t.Fatal(e)
	}
	defer closeSession(t, s)
	a := submit(t, s)
	waitState(t, s, a.TraceID, "completed")
	b := submit(t, s)
	waitState(t, s, b.TraceID, "completed")
	inputs := m.requests()
	if len(inputs) != 2 || len(inputs[1]) != 4 || inputs[1][1].Role != schema.AgenticRoleTypeUser || inputs[1][2].Role != schema.AgenticRoleTypeAssistant || inputs[1][3].Role != schema.AgenticRoleTypeUser {
		t.Fatalf("complete history not projected; request count=%d", len(inputs))
	}
	for _, tr := range manager.View().Traces {
		if tr.Usage.LogicalModelCalls != 1 || tr.Usage.TransportRequests != 1 {
			t.Fatalf("trace budget=%+v", tr.Usage)
		}
	}
}
func TestRuntimePublishesOriginalDurableEvents(t *testing.T) {
	s, manager := runtimeSession(t, &controlledModel{})
	// Start may have committed initialization events before live subscription.
	initialCursor := manager.View().Cursor
	sub := s.SubscribeEvents(config.DefaultLimits())
	defer sub.Close()
	r := submit(t, s)
	deadline := time.After(3 * time.Second)
	got := map[string]agent.Event{}
	for {
		select {
		case ev, ok := <-sub.Events:
			if !ok {
				t.Fatalf("subscription closed: %v", sub.Err())
			}
			if ev.Type == "message.started" || ev.Type == "message.snapshot" {
				if ev.EventID != "" || ev.DurableSeq != nil || ev.StreamID == "" || ev.ChunkSeq == nil {
					t.Fatal("temporary model event has invalid identity")
				}
				continue
			}
			if ev.EventID == "" || ev.DurableSeq == nil {
				t.Fatal("published event has no durable identity")
			}
			got[ev.EventID] = ev
			if ev.Type == "trace.settled" && ev.Scope.TraceID == r.TraceID {
				goto settled
			}
		case <-deadline:
			t.Fatal("no settled event")
		}
	}
settled:
	for _, ev := range manager.View().Events {
		if ev.Type == "queue.changed" || (ev.DurableSeq != nil && *ev.DurableSeq <= initialCursor) {
			continue
		}
		actual, ok := got[ev.EventID]
		if !ok {
			t.Errorf("event not published: %s", ev.Type)
			continue
		}
		if *actual.DurableSeq != *ev.DurableSeq || string(actual.Payload) != string(ev.Payload) {
			t.Error("published event differs from committed event")
		}
	}
}
