package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"

	"github.com/gofrs/flock"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

func boundStoreRoots(t *testing.T, state, id string, create bool) *store.ResourceRoots {
	t.Helper()
	roots, err := store.OpenResourceRoots(state, store.ResourceCode, id, create, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = roots.Close() })
	return roots
}

func boundStoreCode(t *testing.T, err error, code string) {
	t.Helper()
	if pe, ok := product.AsError(err); !ok || pe.Code != code {
		t.Fatalf("error must be direct %s, got %v", code, err)
	}
}

func boundStoreRootsLive(t *testing.T, roots *store.ResourceRoots) {
	t.Helper()
	for _, root := range []*os.Root{roots.Resource, roots.Namespace} {
		if _, err := root.Stat("."); err != nil {
			t.Fatal("caller roots were closed:", err)
		}
	}
}

func boundStoreFileClosed(t *testing.T, file *os.File) {
	t.Helper()
	if _, err := file.Stat(); err == nil {
		t.Fatal("owned file remained open")
	}
	if err := file.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("owned file Close was not already completed:", err)
	}
}

func TestOpenWrapperFailureClosesItsOwnRoots(t *testing.T) {
	state, _ := fileBoundaryJournal(t)
	var ownedRoots []*os.Root
	var opened *os.File
	calls := 0
	s, err := openWithFile("boundary", state, store.Header{}, Options{OpenExisting: true}, func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
		calls++
		var err error
		opened, err = boundOpenFile(root, name, flags, mode)
		if err != nil {
			return opened, err
		}
		return opened, errors.New("private reader failure")
	}, func(parent *os.Root, name string) (*os.Root, error) {
		root, err := parent.OpenRoot(name)
		if root != nil {
			ownedRoots = append(ownedRoots, root)
		}
		return root, err
	})
	boundStoreCode(t, err, product.CodeStorageUnavailable)
	if s != nil || calls != 1 || len(ownedRoots) != 2 || opened == nil {
		t.Fatal("wrapper failure did not enter actual roots/reader core")
	}
	boundStoreFileClosed(t, opened)
	for _, root := range ownedRoots {
		if _, err := root.Stat("."); err == nil {
			t.Fatal("public wrapper abandoned its own root on failure")
		}
	}
}

func TestOpenBoundOwnedInspectionFailureAttemptsRemainingCleanup(t *testing.T) {
	state, dir := fileBoundaryJournal(t)
	roots := boundStoreRoots(t, state, "boundary", false)
	var inspection, final *os.File
	calls := 0
	s, err := openBoundWithFile("boundary", roots, nil, store.Header{}, Options{OpenExisting: true}, func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
		calls++
		if calls == 2 {
			if err := inspection.Close(); err != nil {
				t.Fatal(err)
			}
		}
		file, err := boundOpenFile(root, name, flags, mode)
		if calls == 1 {
			inspection = file
		} else {
			final = file
		}
		return file, err
	})
	boundStoreCode(t, normalizeBoundError(err), product.CodeStorageUnavailable)
	if s != nil || calls != 2 || final == nil || !errors.Is(err, os.ErrClosed) {
		t.Fatal("owned-inspection failure lost real cleanup Close error")
	}
	boundStoreFileClosed(t, inspection)
	boundStoreFileClosed(t, final)
	boundStoreRootsLive(t, roots)
	boundLockProbe(t, dir, "legacy-available")
}

func TestOpenBoundTransfersRootsButNeverInspection(t *testing.T) {
	state, dir := fileBoundaryJournal(t)
	for _, readOnly := range []bool{true, false} {
		t.Run(map[bool]string{true: "reader", false: "writer"}[readOnly], func(t *testing.T) {
			roots := boundStoreRoots(t, state, "boundary", false)
			inspected, err := OpenJournalReader(roots.Resource)
			if err != nil {
				t.Fatal(err)
			}
			defer inspected.Close()
			if inspected.Name() != "journal.jsonl" && runtime.GOOS == "windows" {
				t.Fatal("native reader name must retain basename")
			}
			if _, err := inspected.Seek(7, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			s, err := OpenBound("boundary", roots, inspected, store.Header{}, Options{ReadOnly: readOnly, OpenExisting: true})
			if err != nil {
				t.Fatal(err)
			}
			if s.ResourceRoot() != roots.Resource {
				t.Fatal("ResourceRoot must borrow the transferred root")
			}
			boundStoreRootsLive(t, roots)
			if position, err := inspected.Seek(0, io.SeekCurrent); err != nil || position != 7 {
				t.Fatal("OpenBound changed caller inspection cursor:", position, err)
			}
			loaded, err := s.Load(context.Background(), "boundary")
			if err != nil || loaded.LastSeq != 1 {
				t.Fatal("same journal did not reload original prefix:", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if s.ResourceRoot() != nil {
				t.Fatal("closed store still exposes a borrowed root")
			}
			for _, root := range []*os.Root{roots.Resource, roots.Namespace} {
				if _, err := root.Stat("."); err == nil {
					t.Fatal("Store.Close retained transferred roots")
				}
			}
			if _, err := inspected.Stat(); err != nil {
				t.Fatal("Store.Close closed caller inspection:", err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "journal.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestOpenBoundRejectsHeaderBeforeAnyWriterSideEffect(t *testing.T) {
	for _, kind := range []store.ResourceType{store.ResourceCode, store.ResourceWorkflow} {
		for _, invalid := range []string{"kind", "id", "version", "unreadable", "empty"} {
			t.Run(string(kind)+"/"+invalid, func(t *testing.T) {
				state := t.TempDir()
				roots, err := store.OpenResourceRoots(state, kind, "bound", true, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer roots.Close()
				h := store.Header{RecordType: "header", FormatVersion: 1, ResourceType: kind}
				if kind == store.ResourceWorkflow {
					h.RunID = "bound"
				} else {
					h.SessionID = "bound"
				}
				switch invalid {
				case "kind":
					if kind == store.ResourceCode {
						h.ResourceType, h.RunID, h.SessionID = store.ResourceWorkflow, "bound", ""
					} else {
						h.ResourceType, h.SessionID, h.RunID = store.ResourceCode, "bound", ""
					}
				case "id":
					h.SessionID, h.RunID = "other", "other"
				case "version":
					h.FormatVersion = 99
				}
				raw, _ := json.Marshal(h)
				raw = append(raw, '\n')
				if invalid == "unreadable" {
					raw = []byte("{bad header}\n")
				} else if invalid == "empty" {
					raw = nil
				}
				path := filepath.Join(roots.Path, "journal.jsonl")
				if err := os.WriteFile(path, raw, 0644); err != nil {
					t.Fatal(err)
				}
				if err := roots.Namespace.Chmod(".", 0755); err != nil {
					t.Fatal(err)
				}
				if err := roots.Resource.Chmod(".", 0755); err != nil {
					t.Fatal(err)
				}
				before := fileBoundaryTree(t, state)
				inspection, err := OpenJournalReader(roots.Resource)
				if err != nil {
					t.Fatal(err)
				}
				defer inspection.Close()
				writes, syncs := 0, 0
				s, err := OpenBound("bound", roots, inspection, store.Header{}, Options{ResourceType: kind, OpenExisting: true,
					Write:    func(*os.File, []byte) (int, error) { writes++; return 0, errors.New("must not write") },
					SyncFile: func(*os.File) error { syncs++; return errors.New("must not sync") },
					SyncDir:  func(string) error { syncs++; return errors.New("must not sync directory") },
				})
				code := product.CodeIncompatibleVersion
				if invalid == "empty" {
					code = product.CodeNotFound
				}
				boundStoreCode(t, err, code)
				if s != nil || writes != 0 || syncs != 0 || !reflect.DeepEqual(before, fileBoundaryTree(t, state)) {
					t.Fatal("rejected header had writer/chmod/create side effects")
				}
				boundStoreRootsLive(t, roots)
				if _, err := inspection.Stat(); err != nil {
					t.Fatal("failure closed caller inspection:", err)
				}
			})
		}
	}
}

func TestOpenBoundInitializationFailureLeavesCallerRootsAndReleasesWriter(t *testing.T) {
	for _, fault := range []string{"short-write", "write", "file-sync", "directory-sync", "reload"} {
		t.Run(fault, func(t *testing.T) {
			roots := boundStoreRoots(t, t.TempDir(), "new", true)
			var journal *os.File
			writes, filesyncs, dirsyncs := 0, 0, 0
			opt := Options{
				Write: func(f *os.File, p []byte) (int, error) {
					journal = f
					writes++
					if fault == "short-write" {
						return f.Write(p[:len(p)-1])
					}
					if fault == "write" {
						return 0, errors.New("write failure")
					}
					if fault == "reload" {
						q := append([]byte(nil), p...)
						q[0] = '!'
						return f.Write(q)
					}
					return f.Write(p)
				},
				SyncFile: func(f *os.File) error {
					filesyncs++
					if fault == "file-sync" {
						return errors.New("sync failure")
					}
					return f.Sync()
				},
				SyncDir: func(path string) error {
					dirsyncs++
					if path != roots.Path {
						t.Fatal("explicit directory hook lost its string contract")
					}
					if fault == "directory-sync" {
						return errors.New("directory failure")
					}
					return store.SyncRoot(roots.Resource)
				},
			}
			s, err := OpenBound("new", roots, nil, store.Header{}, opt)
			code := product.CodeStorageUnavailable
			if fault == "reload" {
				code = product.CodeIncompatibleVersion
			}
			boundStoreCode(t, err, code)
			if s != nil || journal == nil || writes != 1 {
				t.Fatal("initialization did not run exactly one real header write")
			}
			if (fault == "short-write" || fault == "write") && (filesyncs != 0 || dirsyncs != 0) {
				t.Fatal("write failure continued to sync")
			}
			if fault == "file-sync" && (filesyncs != 1 || dirsyncs != 0) {
				t.Fatal("file sync failure continued to directory sync")
			}
			boundStoreFileClosed(t, journal)
			boundStoreRootsLive(t, roots)
			legacy := flock.New(filepath.Join(roots.Path, "writer.lock"))
			ok, err := legacy.TryLock()
			if err != nil || !ok {
				t.Fatal("constructor failure retained private writer")
			}
			if err := legacy.Unlock(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenBoundDefaultSyncIgnoresDiagnosticPathAndClosesOwnInspection(t *testing.T) {
	state := t.TempDir()
	roots := boundStoreRoots(t, state, "new", true)
	roots.Path = filepath.Join(t.TempDir(), "diagnostic-only-does-not-exist")
	s, err := OpenBound("new", roots, nil, store.Header{}, Options{})
	if err != nil {
		t.Fatal("default journal sync must use Resource root, not Path:", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	roots = boundStoreRoots(t, state, "new", false)
	var inspected *os.File
	opens := 0
	s, err = openBoundWithFile("new", roots, nil, store.Header{}, Options{OpenExisting: true}, func(root *os.Root, name string, flags int, perm os.FileMode) (*os.File, error) {
		opens++
		if opens == 2 {
			if _, err := inspected.Stat(); err != nil {
				t.Fatal("owned inspection was closed before final journal open")
			}
		}
		file, err := boundOpenFile(root, name, flags, perm)
		if opens == 1 {
			inspected = file
		}
		return file, err
	})
	if err != nil || s == nil || opens != 2 {
		t.Fatal("existing bound core did not use reader and final opener:", opens, err)
	}
	boundStoreFileClosed(t, inspected)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenBoundNewCollisionNeverTruncates(t *testing.T) {
	roots := boundStoreRoots(t, t.TempDir(), "new", true)
	calls := 0
	sentinel := []byte("collision must stay unchanged")
	s, err := openBoundWithFile("new", roots, nil, store.Header{}, Options{}, func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
		calls++
		if flags&os.O_EXCL == 0 || flags&os.O_CREATE == 0 || name != "journal.jsonl" {
			t.Fatal("new journal must use finite CREATE|EXCL")
		}
		f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(sentinel); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		return boundOpenFile(root, name, flags, mode)
	})
	boundStoreCode(t, normalizeBoundError(err), product.CodeStorageUnavailable)
	if s != nil || calls != 1 {
		t.Fatal("collision returned store or retried another open")
	}
	raw, readErr := roots.Resource.ReadFile("journal.jsonl")
	if readErr != nil || !bytes.Equal(raw, sentinel) {
		t.Fatal("collision truncated journal:", readErr)
	}
	boundStoreRootsLive(t, roots)
	legacy := flock.New(filepath.Join(roots.Path, "writer.lock"))
	if ok, err := legacy.TryLock(); err != nil || !ok {
		t.Fatal("collision leaked lock")
	}
	if err := legacy.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenBoundFinalJournalKeepsInspectionIdentity(t *testing.T) {
	for _, borrowed := range []bool{true, false} {
		t.Run(map[bool]string{true: "caller-inspection", false: "owned-inspection"}[borrowed], func(t *testing.T) {
			state, dir := fileBoundaryJournal(t)
			roots := boundStoreRoots(t, state, "boundary", false)
			var inspected *os.File
			if borrowed {
				var err error
				inspected, err = OpenJournalReader(roots.Resource)
				if err != nil {
					t.Fatal(err)
				}
				defer inspected.Close()
			}
			external := filepath.Join(t.TempDir(), "journal.jsonl")
			raw, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(external, raw, 0644); err != nil {
				t.Fatal(err)
			}
			calls, denied := 0, false
			s, openErr := openBoundWithFile("boundary", roots, inspected, store.Header{}, Options{OpenExisting: true}, func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
				calls++
				if flags == os.O_RDONLY {
					return boundOpenFile(root, name, flags, mode)
				}
				path := filepath.Join(dir, name)
				renameErr := os.Rename(path, path+".held")
				if runtime.GOOS == "windows" {
					if !errors.Is(renameErr, syscall.Errno(32)) && !errors.Is(renameErr, syscall.Errno(5)) {
						t.Fatalf("held inspection must cause actual denyDELETE, got %v", renameErr)
					}
					denied = true
				} else {
					if renameErr != nil {
						t.Fatal(renameErr)
					}
					if err := os.Link(external, path); err != nil {
						t.Fatal(err)
					}
				}
				return boundOpenFile(root, name, flags, mode)
			})
			wantCalls := 2
			if borrowed {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatal("wrong actual journal open count:", calls)
			}
			if runtime.GOOS == "windows" {
				if !denied || openErr != nil || s == nil {
					t.Fatal("OS-denied replacement lost original binding:", openErr)
				}
				original, _ := os.Stat(filepath.Join(dir, "journal.jsonl"))
				opened, _ := s.journal.Stat()
				if !os.SameFile(original, opened) {
					t.Fatal("bound writer changed original identity")
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				boundStoreCode(t, normalizeBoundError(openErr), product.CodeInvalidArgument)
				if s != nil {
					t.Fatal("final writer accepted inode different from inspected")
				}
				boundStoreRootsLive(t, roots)
			}
			if borrowed {
				if _, err := inspected.Stat(); err != nil {
					t.Fatal("final identity path closed borrowed inspection")
				}
			}
			after, err := os.ReadFile(external)
			if err != nil || !bytes.Equal(after, raw) {
				t.Fatal("final writer changed external bytes:", err)
			}
		})
	}
}
