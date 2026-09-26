package sessions_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type failingCheckpointStore struct {
	store.Store
	store.CheckpointBlobs
}

func (s failingCheckpointStore) Put(context.Context, string, []byte) (store.BlobRef, error) {
	return store.BlobRef{}, errors.New("injected blob failure")
}

func TestPauseBlobFailureNeverAssociatesCheckpoint(t *testing.T) {
	backend, err := memory.Open("pause-blob-failure", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	faults := failingCheckpointStore{Store: backend, CheckpointBlobs: backend}
	manager, err := state.NewManager(faults, "pause-blob-failure")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var runs atomic.Int32
	model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "not reached"})
	s, err := sessions.Start(sessions.Options{SessionID: "pause-blob-failure", Profile: sessions.ProfileMemory, Store: faults, Model: model, Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		close(entered)
		<-release
		return "ok", nil
	}}}, ToolInfos: []*schema.ToolInfo{testkit.ToolInfo("work", "work")}}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, s)
	receipt := submit(t, s)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("tool not entered")
	}
	result := make(chan error, 1)
	go func() { _, err := s.Pause(t.Context(), receipt.TraceID); result <- err }()
	deadline := time.After(3 * time.Second)
	for len(manager.View().Operations) == 0 {
		select {
		case <-deadline:
			t.Fatal("operation not accepted")
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("blob failure reported successful pause")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause never exited")
	}
	view := manager.View()
	if len(view.Checkpoints) != 0 || view.Traces[receipt.TraceID].State == "paused" || runs.Load() != 1 || model.Calls() != 1 {
		t.Fatalf("unsafe checkpoint: trace=%+v checkpoints=%d tools=%d models=%d", view.Traces[receipt.TraceID], len(view.Checkpoints), runs.Load(), model.Calls())
	}
}
