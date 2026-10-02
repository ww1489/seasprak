package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// Acceptance tests use the actual relative child and leaf windows of the Store.
func TestFileBoundaryJournalRejectsReplacement(t *testing.T) {
	for _, component := range []string{"sessions", "session", "journal-regular", "journal-link"} {
		for _, readOnly := range []bool{true, false} {
			mode := "reader"
			if !readOnly {
				mode = "writer"
			}
			t.Run(component+"/"+mode, func(t *testing.T) {
				root, dir := fileBoundaryJournal(t)
				outside := t.TempDir()
				link, externalDir := filepath.Join(dir, "journal.jsonl"), outside
				switch component {
				case "sessions":
					link, externalDir = filepath.Dir(dir), filepath.Join(outside, "boundary")
				case "session":
					link = dir
				}
				if err := os.MkdirAll(externalDir, 0755); err != nil {
					t.Fatal(err)
				}
				header := store.Header{RecordType: "header", FormatVersion: 1, ResourceType: store.ResourceCode, SessionID: "boundary", Workspace: json.RawMessage(`{"fixture":"external"}`)}
				raw, err := json.Marshal(header)
				if err != nil {
					t.Fatal(err)
				}
				external := filepath.Join(externalDir, "journal.jsonl")
				if err := os.WriteFile(external, append(raw, '\n'), 0644); err != nil {
					t.Fatal(err)
				}
				before := fileBoundaryTree(t, outside)
				reached, resume := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(resume) }) }
				type result struct {
					store *Store
					err   error
				}
				done := make(chan result, 1)
				finished, opens, writes, syncs := false, 0, 0, 0
				go func() {
					s, err := openWithFile("boundary", root, store.Header{}, Options{OpenExisting: true, ReadOnly: readOnly,
						Write:    func(f *os.File, p []byte) (int, error) { writes++; return f.Write(p) },
						SyncFile: func(f *os.File) error { syncs++; return f.Sync() },
					}, func(bound *os.Root, name string, flags int, perm os.FileMode) (*os.File, error) {
						if component == "journal-regular" || component == "journal-link" {
							opens++
							close(reached)
							<-resume
						}
						return boundOpenFile(bound, name, flags, perm)
					}, func(parent *os.Root, name string) (*os.Root, error) {
						if component == "sessions" && name == "sessions" || component == "session" && name == "boundary" {
							opens++
							close(reached)
							<-resume
						}
						return parent.OpenRoot(name)
					})
					done <- result{s, err}
				}()
				t.Cleanup(func() {
					unblock()
					if !finished {
						select {
						case got := <-done:
							if got.store != nil {
								_ = got.store.Close()
							}
						case <-time.After(10 * time.Second):
							t.Error("journal opener did not exit")
						}
					}
				})
				select {
				case <-reached:
				case <-time.After(10 * time.Second):
					t.Fatal("journal opener did not reach boundary")
				}
				if err := os.Rename(link, link+".held"); err != nil {
					t.Fatal("replace checked path:", err)
				}
				switch component {
				case "sessions", "session":
					fileBoundaryDirectoryLink(t, link, outside)
				case "journal-link":
					fileBoundaryFileLink(t, link, external)
				case "journal-regular":
					// A hard link supplies the exact external inode under a regular leaf.
					// This fixture tests identity replacement, not general hard-link isolation.
					if err := os.Link(external, link); err != nil {
						t.Fatal("regular replacement fixture:", err)
					}
				}
				unblock()
				var got result
				select {
				case got = <-done:
					finished = true
				case <-time.After(10 * time.Second):
					t.Fatal("journal opener did not return")
				}
				if opens != 1 {
					t.Errorf("real journal open calls=%d, want 1", opens)
				}
				if pe, ok := product.AsError(got.err); !ok || pe.Code != product.CodeInvalidArgument {
					t.Errorf("replacement journal error=%v, want invalid_argument", got.err)
				}
				if got.store != nil {
					loaded, err := got.store.Load(context.Background(), "boundary")
					if err != nil {
						t.Error("load replacement:", err)
					} else {
						t.Errorf("opened replacement journal: workspace=%s seq=%d", loaded.Header.Workspace, loaded.LastSeq)
					}
					if !readOnly {
						if _, err := got.store.Append(context.Background(), "boundary", store.ExpectedCommit{}, store.Commit{CommitID: "escaped"}); err != nil {
							t.Error("append replacement:", err)
						}
					}
					if err := got.store.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if writes != 0 || syncs != 0 {
					t.Errorf("replacement backend calls: writes=%d syncs=%d, want zero", writes, syncs)
				}
				if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
					t.Error("journal access changed external bytes, modes or entries")
				}
			})
		}
	}
}

func TestFileBoundaryWriterLockRejectsStaticLink(t *testing.T) {
	root, dir := fileBoundaryJournal(t)
	outside := t.TempDir()
	external := filepath.Join(outside, "outside.lock")
	if err := os.WriteFile(external, []byte("external lock sentinel"), 0644); err != nil {
		t.Fatal(err)
	}
	fileBoundaryFileLink(t, filepath.Join(dir, "writer.lock"), external)
	before := fileBoundaryTree(t, outside)
	s, err := Open("boundary", root, store.Header{}, Options{OpenExisting: true})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
		t.Errorf("linked writer lock error=%v, want invalid_argument", err)
	}
	if s != nil {
		competitor := flock.New(external)
		ok, lockErr := competitor.TryLock()
		if lockErr != nil {
			t.Error("probe external lock:", lockErr)
		} else if !ok {
			t.Error("session acquired the external target's lock")
		}
		if err := competitor.Unlock(); err != nil {
			t.Error(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if after := fileBoundaryTree(t, outside); !reflect.DeepEqual(before, after) {
		t.Error("writer lock access changed external bytes, modes or entries")
	}
}

func TestFileBoundaryWriterLockRejectsNonregular(t *testing.T) {
	for _, kind := range []string{"directory", "directory-link"} {
		t.Run(kind, func(t *testing.T) {
			root, dir := fileBoundaryJournal(t)
			outside := t.TempDir()
			lock := filepath.Join(dir, "writer.lock")
			if kind == "directory" {
				if err := os.Mkdir(lock, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				fileBoundaryDirectoryLink(t, lock, outside)
			}
			before := fileBoundaryTree(t, outside)
			s, err := Open("boundary", root, store.Header{}, Options{OpenExisting: true})
			if s != nil {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
				t.Errorf("nonregular writer lock error=%v, want invalid_argument", err)
			}
			if after := fileBoundaryTree(t, outside); !reflect.DeepEqual(before, after) {
				t.Error("nonregular lock access changed external bytes, modes or entries")
			}
		})
	}
}

func fileBoundaryJournal(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	s, err := Open("boundary", root, store.Header{}, Options{})
	if err != nil {
		t.Fatal("create journal fixture:", err)
	}
	if _, err := s.Append(context.Background(), "boundary", store.ExpectedCommit{}, store.Commit{CommitID: "fixture"}); err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "sessions", "boundary")
	if err := os.Remove(filepath.Join(dir, "writer.lock")); err != nil {
		t.Fatal(err)
	}
	return root, dir
}

func fileBoundaryFileLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
			t.Skip("Windows file symlink privilege unavailable; file-link behavior unverified")
		}
		t.Fatal("file symlink fixture:", err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}

func fileBoundaryDirectoryLink(t *testing.T, link, target string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		command, err := exec.LookPath("cmd.exe")
		if err != nil {
			t.Skip("cmd.exe unavailable; directory junction behavior unverified")
		}
		if out, err := exec.Command(command, "/d", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("directory junction fixture: %v: %s", err, out)
		}
	} else if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}

type fileBoundaryEntry struct {
	Mode os.FileMode
	Data []byte
}

func fileBoundaryTree(t *testing.T, dir string) map[string]fileBoundaryEntry {
	t.Helper()
	out := map[string]fileBoundaryEntry{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		saved := fileBoundaryEntry{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			saved.Data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
			saved.Data = bytes.Clone(saved.Data)
		}
		out[rel] = saved
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
