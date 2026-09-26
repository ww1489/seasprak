package sessions_test

import (
	"context"
	"testing"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

type noCheckpointBlobs struct{ store.Store }

func TestPauseWithoutBlobCapabilityFailsBeforeAcceptingOperation(t *testing.T) {
	backend, err := memory.Open("pause-no-blobs", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := noCheckpointBlobs{backend}
	manager, err := state.NewManager(wrapped, "pause-no-blobs")
	if err != nil {
		t.Fatal(err)
	}
	model := &controlledModel{entered: make(chan context.Context, 1), gate: make(chan struct{})}
	defer close(model.gate)
	s, err := sessions.Start(sessions.Options{SessionID: "pause-no-blobs", Profile: sessions.ProfileMemory, Store: wrapped, Model: model}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, s)
	receipt := submit(t, s)
	select {
	case <-model.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("model not started")
	}
	_, err = s.Pause(t.Context(), receipt.TraceID)
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeStorageUnavailable || len(manager.View().Operations) != 0 || manager.View().Traces[receipt.TraceID].State != "running" {
		t.Fatalf("no blob capability accepted pause: %v operations=%d trace=%+v", err, len(manager.View().Operations), manager.View().Traces[receipt.TraceID])
	}
}
