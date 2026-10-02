package codeagent

import (
	"context"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestCodeInjectedHeaderPreservesWorkspaceBinding(t *testing.T) {
	for _, rebind := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching", true: "rebound"}[rebind], func(t *testing.T) {
			bound := t.TempDir()
			workspace := bound
			if rebind {
				workspace = t.TempDir()
			}
			backend, err := memory.Open("injected-code", storage.Header{ResourceType: storage.ResourceCode, Workspace: workspaceJSON(bound, "")})
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			m := testkit.NewFake()
			s, err := CreateAgentSession(t.Context(), Options{SessionID: "injected-code", Workspace: workspace, StateRoot: "memory", Profile: ProfileMemory, Store: backend, Model: m})
			if s != nil {
				defer s.Close(context.Background())
			}
			loaded, loadErr := backend.Load(t.Context(), "injected-code")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if rebind {
				if s != nil {
					t.Error("injected header workspace was rebound")
				}
				if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
					t.Errorf("error=%v want incompatible_version", err)
				}
				if len(loaded.Commits) != 0 {
					t.Errorf("rejected binding wrote %d commits", len(loaded.Commits))
				}
			} else if err != nil || s == nil || len(loaded.Commits) == 0 {
				t.Errorf("legal injected Code header rejected: %v", err)
			}
			if m.Calls() != 0 {
				t.Fatal("factory executed model")
			}
		})
	}
}
