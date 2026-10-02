package state_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
)

func TestHostCommandFactDiskReopenAndReadOnly(t *testing.T) {
	root := t.TempDir()
	s, err := jsonl.Open("session", root, store.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	m, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	// Existing shell history is readable as-is, alongside the new facts.
	if err := m.AppendMessage(t.Context(), hostResult("legacy")); err != nil {
		t.Fatal(err)
	}
	legacy := m.View()
	for _, exclude := range []bool{false, true} {
		id := map[bool]string{false: "included", true: "excluded"}[exclude]
		if err := m.SaveHostCommand(t.Context(), hostResult(id), exclude); err != nil {
			t.Fatal(err)
		}
	}
	before := m.View()
	if before.LeafID != legacy.LeafID || !reflect.DeepEqual(before.Messages, legacy.Messages) || len(before.HostCommands) != 2 {
		t.Fatal("new facts changed legacy history")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	files := hostFactDiskImage(t, root)
	for i := 0; i < 2; i++ {
		reader, err := jsonl.Open("session", root, store.Header{}, jsonl.Options{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reader.Close() })
		reopened, err := state.NewManager(reader, "session")
		if err != nil || !reflect.DeepEqual(reopened.View(), before) {
			t.Fatal("read-only disk replay changed facts or legacy history", err)
		}
		if err := reopened.SaveHostCommand(t.Context(), hostResult("included"), false); err != nil {
			t.Fatal("duplicate result attempted a read-only write", err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(files, hostFactDiskImage(t, root)) {
		t.Fatal("read-only reopen wrote files, changed permissions or metadata")
	}
}

type hostFactFileImage struct {
	Content string
	Mode    fs.FileMode
	ModTime time.Time
}

func hostFactDiskImage(t *testing.T, root string) map[string]hostFactFileImage {
	t.Helper()
	out := make(map[string]hostFactFileImage)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		image := hostFactFileImage{Mode: info.Mode(), ModTime: info.ModTime()}
		if !entry.IsDir() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			image.Content = string(body)
		}
		out[path] = image
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestHostCommandFactDiskStorageFailure(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "lost-ack"}[lost], func(t *testing.T) {
			root := t.TempDir()
			s, err := jsonl.Open("session", root, store.Header{}, jsonl.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			m, err := state.NewManager(&hostResultFaultStore{Store: s, lost: lost}, "session")
			if err != nil {
				t.Fatal(err)
			}
			before := m.View()
			requireP2Code(t, m.SaveHostCommand(t.Context(), hostResult("shell"), false), product.CodeStorageUnavailable)
			if !reflect.DeepEqual(m.View(), before) || m.Fault() == nil {
				t.Fatal("failed save published a fact or left the writer usable")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopenedStore, err := jsonl.Open("session", root, store.Header{}, jsonl.Options{OpenExisting: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopenedStore.Close() })
			reopened, err := state.NewManager(reopenedStore, "session")
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if lost {
				want = 1
			}
			view := reopened.View()
			if len(view.HostCommands) != want || len(view.Messages) != 0 || view.LeafID != "" {
				t.Fatal("disk reopen lost an acknowledged-by-store fact or invented history")
			}
			if lost {
				if !reflect.DeepEqual(view.HostCommands["shell"].Message, hostResult("shell")) {
					t.Fatal("disk lost-ack recovery changed the original result")
				}
				if err := reopened.SaveHostCommand(t.Context(), hostResult("shell"), false); err != nil || !reflect.DeepEqual(view, reopened.View()) {
					t.Fatal("re-saving the recovered result duplicated it", err)
				}
			}
		})
	}
}
