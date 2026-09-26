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

type associationFailureStore struct {
	store.Store
	store.CheckpointBlobs
	puts atomic.Int32
}

func (s *associationFailureStore) Put(ctx context.Context, id string, data []byte) (store.BlobRef, error) {
	s.puts.Add(1)
	return s.CheckpointBlobs.Put(ctx, id, data)
}
func (s *associationFailureStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	for _, r := range commit.ControlRecords {
		if r.Type == "checkpoint_ref" {
			return store.CommitReceipt{}, errors.New("injected association failure")
		}
	}
	return s.Store.Append(ctx, id, expected, commit)
}

func TestPauseAssociationFailureLeavesOrphanBlobButNoRecoverableTrace(t *testing.T) {
	backend, err := memory.Open("pause-association-failure", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	faults := &associationFailureStore{Store: backend, CheckpointBlobs: backend}
	manager, err := state.NewManager(faults, "pause-association-failure")
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
	s, err := sessions.Start(sessions.Options{SessionID: "pause-association-failure", Profile: sessions.ProfileMemory, Store: faults, Model: model, Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
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
	go func() { _, err := s.Pause(context.Background(), receipt.TraceID); result <- err }()
	deadline := time.After(3 * time.Second)
	for len(manager.View().Operations) == 0 {
		select {
		case <-deadline:
			t.Fatal("pause not accepted")
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("failed association reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not exit")
	}
	view := manager.View()
	if faults.puts.Load() != 1 || len(view.Checkpoints) != 0 || view.Traces[receipt.TraceID].State == "paused" || runs.Load() != 1 || model.Calls() != 1 {
		t.Fatalf("orphan claimed recoverable: puts=%d checkpoints=%d trace=%+v tools=%d model=%d", faults.puts.Load(), len(view.Checkpoints), view.Traces[receipt.TraceID], runs.Load(), model.Calls())
	}
	replayed, err := state.NewManager(backend, "pause-association-failure")
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.View().Checkpoints) != 0 || replayed.View().Traces[receipt.TraceID].State == "paused" {
		t.Fatal("failed commit replayed checkpoint")
	}
}
