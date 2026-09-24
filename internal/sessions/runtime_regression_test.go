package sessions_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

type controlledModel struct {
	entered      chan context.Context
	gate         chan struct{}
	failure      error
	ignoreCancel bool
}

func (m *controlledModel) Generate(ctx context.Context, _ []*schema.AgenticMessage, _ ...einomodel.Option) (*schema.AgenticMessage, error) {
	if m.entered != nil {
		m.entered <- ctx
	}
	if m.gate != nil {
		if m.ignoreCancel {
			<-m.gate
		} else {
			select {
			case <-m.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if m.failure != nil {
		return nil, m.failure
	}
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "done"})}, Extra: map[string]any{"seasprak.finish": "stop"}}, nil
}
func (m *controlledModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	v, e := m.Generate(ctx, in, opts...)
	if e != nil {
		return nil, e
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{v}), nil
}
func runtimeSession(t *testing.T, m einomodel.AgenticModel) (*sessions.AgentSession, *state.Manager) {
	t.Helper()
	store, _ := memory.Open("regression", store.Header{})
	manager, e := state.NewManager(store, "regression")
	if e != nil {
		t.Fatal(e)
	}
	s, e := sessions.Start(sessions.Options{SessionID: "regression", Profile: sessions.ProfileMemory, Model: m, Store: store}, manager, "gen")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if e := s.Close(ctx); e != nil {
			t.Error(e)
		}
	})
	return s, manager
}
func submit(t *testing.T, s *sessions.AgentSession) agent.InputReceipt {
	t.Helper()
	r, e := s.SubmitInput(context.Background(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hi"}`)})
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func awaitStart(t *testing.T, m *controlledModel) context.Context {
	t.Helper()
	select {
	case c := <-m.entered:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("model never started")
		return nil
	}
}
func TestRuntimeModelFailureIsNotCompletion(t *testing.T) {
	s, _ := runtimeSession(t, &controlledModel{failure: errors.New("model failed")})
	r := submit(t, s)
	waitState(t, s, r.TraceID, "failed")
}
func TestRuntimePromptQueuesAndCancelStops(t *testing.T) {
	m := &controlledModel{entered: make(chan context.Context, 8), gate: make(chan struct{})}
	s, _ := runtimeSession(t, m)
	defer close(m.gate)
	first := submit(t, s)
	runCtx := awaitStart(t, m)
	second := submit(t, s)
	snap, _ := s.Snapshot(context.Background())
	if snap.Traces[second.TraceID].State != "queued" {
		t.Error("second prompt was not queued")
	}
	select {
	case <-m.entered:
		t.Error("second model overlapped first")
	case <-time.After(50 * time.Millisecond):
	}
	if e := s.Cancel(context.Background(), first.TraceID); e != nil {
		t.Fatal(e)
	}
	select {
	case <-runCtx.Done():
	default:
		t.Error("cancel did not cancel model context")
	}
	waitState(t, s, first.TraceID, "cancelled")
	snap, _ = s.Snapshot(context.Background())
	if !snap.Traces[second.TraceID].Hold {
		t.Error("preexisting queued prompt was not held")
	}
}
func TestRuntimeFollowUpExecutes(t *testing.T) {
	m := &controlledModel{entered: make(chan context.Context, 8), gate: make(chan struct{})}
	s, _ := runtimeSession(t, m)
	r := submit(t, s)
	awaitStart(t, m)
	follow, e := s.SubmitInput(context.Background(), agent.InputCommand{Kind: "follow_up", TargetTraceID: r.TraceID, Content: json.RawMessage(`{"text":"continue"}`)})
	if e != nil {
		t.Fatal(e)
	}
	close(m.gate)
	awaitStart(t, m)
	waitState(t, s, r.TraceID, "completed")
	snap, _ := s.Snapshot(context.Background())
	if snap.Inputs[follow.InputID].State != "consumed" {
		t.Error("follow-up not consumed")
	}
}
func TestRuntimeCloseWaitsAndRejectsWrites(t *testing.T) {
	m := &controlledModel{entered: make(chan context.Context, 8), gate: make(chan struct{}), ignoreCancel: true}
	s, _ := runtimeSession(t, m)
	submit(t, s)
	runCtx := awaitStart(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	e := s.Close(ctx)
	if e == nil {
		t.Error("close succeeded while model was blocked")
	}
	select {
	case <-runCtx.Done():
	default:
		t.Error("close did not request cancellation")
	}
	_, e = s.SubmitInput(context.Background(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{}`)})
	if e == nil {
		t.Error("closing session accepted a write")
	}
	close(m.gate)
	if e = s.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestRuntimeSnapshotDoesNotAlias(t *testing.T) {
	s, _ := runtimeSession(t, &controlledModel{})
	r := submit(t, s)
	waitState(t, s, r.TraceID, "completed")
	v, _ := s.Snapshot(context.Background())
	v.Traces[r.TraceID].State = "tampered"
	v.Inputs[r.InputID].Content[0] = 'x'
	next, _ := s.Snapshot(context.Background())
	if next.Traces[r.TraceID].State != "completed" || !json.Valid(next.Inputs[r.InputID].Content) {
		t.Error("snapshot aliases session state")
	}
}
