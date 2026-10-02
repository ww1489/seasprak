package codeagent

import (
	"context"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestCodeCreateRejectsInjectedWorkflowStoreBeforeAnyAppend(t *testing.T) {
	id := "injected-cross-type"
	backend, err := memory.Open(id, store.Header{ResourceType: store.ResourceWorkflow, RunID: id})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	s, err := CreateAgentSession(t.Context(), Options{Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, SessionID: id, Model: testkit.NewFake(), Store: backend})
	if s != nil {
		defer s.Close(context.Background())
	}
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
		t.Errorf("injected cross-type error %v", err)
	}
	loaded, err := backend.Load(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastSeq != 0 {
		t.Errorf("cross-type create appended %d commits", loaded.LastSeq)
	}
}
