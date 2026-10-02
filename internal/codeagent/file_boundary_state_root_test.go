package codeagent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

func TestFileBoundaryStateRootHeaderAndInspectClassification(t *testing.T) {
	for _, input := range []string{"empty", "missing", "file", "missing-namespace", "missing-session"} {
		t.Run(input, func(t *testing.T) {
			fixture, workspace := t.TempDir(), t.TempDir()
			stateRoot := ""
			code := product.CodeIncompatibleVersion
			message := "session header path is invalid"
			if input != "empty" {
				stateRoot = filepath.Join(fixture, "state")
			}
			switch input {
			case "file":
				if err := os.WriteFile(stateRoot, []byte("not a state directory"), 0644); err != nil {
					t.Fatal(err)
				}
			case "missing-namespace", "missing-session":
				if err := os.Mkdir(stateRoot, 0755); err != nil {
					t.Fatal(err)
				}
				if input == "missing-session" {
					if err := os.Mkdir(filepath.Join(stateRoot, "sessions"), 0755); err != nil {
						t.Fatal(err)
					}
				}
				code = product.CodeNotFound
				message = "session does not exist"
			}
			before, workspaceBefore := fileBoundaryTree(t, fixture), fileBoundaryTree(t, workspace)
			assertUnchanged := func(t *testing.T) {
				t.Helper()
				if !reflect.DeepEqual(before, fileBoundaryTree(t, fixture)) || !reflect.DeepEqual(workspaceBefore, fileBoundaryTree(t, workspace)) {
					t.Error("read-only rejection changed bytes, modes or entries")
				}
			}
			t.Run("readHeader", func(t *testing.T) {
				header, err := readHeader(stateRoot, "bound", 4096)
				if !reflect.DeepEqual(header, store.Header{}) {
					t.Error("rejection returned a header")
				}
				assertStateRootError(t, err, code, message)
				assertUnchanged(t)
			})
			t.Run("InspectSessionHeader", func(t *testing.T) {
				descriptor, err := InspectSessionHeader(context.Background(), stateRoot, "bound", config.Limits{})
				if descriptor != (SessionDescriptor{}) {
					t.Error("rejection returned a descriptor")
				}
				assertStateRootError(t, err, code, message)
				assertUnchanged(t)
			})
			t.Run("OpenAgentSession", func(t *testing.T) {
				session, err := OpenAgentSession(context.Background(), Options{StateRoot: stateRoot, SessionID: "bound", Workspace: workspace, ReadOnly: true})
				if session != nil {
					_ = session.Close(context.Background())
					t.Error("rejection returned a session")
				}
				openCode := code
				if code == product.CodeIncompatibleVersion {
					openCode = product.CodeInvalidArgument
				}
				if pe, ok := product.AsError(err); !ok || pe.Code != openCode {
					t.Errorf("error=%v, want %s", err, openCode)
				}
				assertUnchanged(t)
			})
		})
	}
}

func assertStateRootError(t *testing.T, err error, code, message string) {
	t.Helper()
	if pe, ok := product.AsError(err); !ok || pe.Code != code || pe.Message != message || pe.Details != nil || len(pe.Refs) != 0 {
		t.Errorf("error=%v, want fixed %s without details or refs", err, code)
	}
}
