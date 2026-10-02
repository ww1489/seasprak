package jsonl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	store "github.com/ww1489/seasprak/internal/storage"
)

func TestOpenBoundCompletedResourceBindingNeverReopensPath(t *testing.T) {
	for _, mode := range []string{"reader", "existing-writer", "new-writer"} {
		t.Run(mode, func(t *testing.T) {
			state := t.TempDir()
			if mode != "new-writer" {
				s, err := Open("bound", state, store.Header{Workspace: json.RawMessage(`{"fixture":"trusted"}`)}, Options{})
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			roots := boundStoreRoots(t, state, "bound", true)
			before, err := roots.Resource.Stat(".")
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "journal.jsonl"), []byte("outside sentinel"), 0644); err != nil {
				t.Fatal(err)
			}
			outsideBefore := fileBoundaryTree(t, outside)
			path := roots.Path
			if err := os.Rename(path, path+".held"); err != nil {
				t.Fatal("move bound resource before any journal/lock open:", err)
			}
			fileBoundaryDirectoryLink(t, path, outside)
			t.Cleanup(func() { _ = os.Remove(path) })
			writes, syncs := 0, 0
			s, err := OpenBound("bound", roots, nil, store.Header{Workspace: json.RawMessage(`{"fixture":"trusted"}`)}, Options{
				ReadOnly: mode == "reader", OpenExisting: mode != "new-writer",
				Write:    func(f *os.File, data []byte) (int, error) { writes++; return f.Write(data) },
				SyncFile: func(f *os.File) error { syncs++; return f.Sync() },
			})
			if err != nil {
				t.Fatal("OpenBound must retain the completed resource binding:", err)
			}
			defer s.Close()
			got, err := s.ResourceRoot().Stat(".")
			if err != nil || !os.SameFile(before, got) {
				t.Fatal("Store changed bound directory identity")
			}
			loaded, err := s.Load(context.Background(), "bound")
			if err != nil || string(loaded.Header.Workspace) != `{"fixture":"trusted"}` {
				t.Fatal("Store did not use original journal/header:", err)
			}
			if mode == "reader" {
				if writes != 0 || syncs != 0 || s.lock != nil {
					t.Fatal("bound reader had writer side effects")
				}
			} else {
				if _, err := s.Append(context.Background(), "bound", store.ExpectedCommit{}, store.Commit{CommitID: "bound-append"}); err != nil {
					t.Fatal(err)
				}
				want := 1
				if mode == "new-writer" {
					want = 2 // Header, then actual Append, each followed by File.Sync.
				}
				if writes != want || syncs != want {
					t.Fatalf("writes=%d syncs=%d want=%d", writes, syncs, want)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(outsideBefore, fileBoundaryTree(t, outside)) {
				t.Fatal("bound journal/lock/chmod/sync/append affected replacement directory")
			}
		})
	}
}
