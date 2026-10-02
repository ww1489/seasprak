package state_test

import (
	"testing"

	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
)

func TestCodeManagerRejectsWorkflowHeaderWithoutWriting(t *testing.T) {
	backend, err := memory.Open("same", storage.Header{ResourceType: storage.ResourceWorkflow, RunID: "same"})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	manager, err := state.NewManager(backend, "same")
	if manager != nil {
		t.Error("Code manager accepted Workflow journal")
	}
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
		t.Errorf("error=%v, want incompatible_version", err)
	}
	loaded, err := backend.Load(t.Context(), "same")
	if err != nil || len(loaded.Commits) != 0 {
		t.Errorf("type validation changed journal: %v commits=%d", err, len(loaded.Commits))
	}
}
