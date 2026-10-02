package jsonl

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

func TestOpenJournalReaderRejectsInvalidLeavesAndKeepsBorrowedRoot(t *testing.T) {
	for _, kind := range []string{"directory", "directory-link", "file-link", "missing"} {
		t.Run(kind, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			path := filepath.Join(root.Name(), "journal.jsonl")
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "external"), []byte("not a journal"), 0644); err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, outside)
			switch kind {
			case "directory":
				if err := root.Mkdir("journal.jsonl", 0700); err != nil {
					t.Fatal(err)
				}
			case "directory-link":
				fileBoundaryDirectoryLink(t, path, outside)
			case "file-link":
				fileBoundaryFileLink(t, path, filepath.Join(outside, "external"))
			}
			calls := 0
			file, err := openJournalReaderWithFile(root, func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
				calls++
				return boundOpenFile(root, name, flags, mode)
			})
			code := product.CodeInvalidArgument
			if kind == "missing" {
				code = product.CodeNotFound
			}
			boundStoreCode(t, normalizeBoundError(err), code)
			if calls != 0 || file != nil {
				t.Fatal("invalid static journal reached native leaf open")
			}
			file, err = OpenJournalReader(root)
			boundStoreCode(t, err, code)
			if file != nil {
				t.Fatal("public reader returned invalid leaf")
			}
			if _, err := root.Stat("."); err != nil || !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
				t.Fatal("reader failure closed Root or changed external tree")
			}
		})
	}
	file, err := OpenJournalReader(nil)
	boundStoreCode(t, err, product.CodeInvalidArgument)
	if file != nil {
		t.Fatal("nil root returned file")
	}
}

func TestBoundLeafValidationClosesNewFileOnEveryFailure(t *testing.T) {
	for _, fault := range []string{"different-file", "directory", "closed-file", "closed-root", "file-and-error", "post-open-replacement"} {
		t.Run(fault, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := root.WriteFile("journal.jsonl", []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			var owned *os.File
			calls, denied := 0, false
			cause := errors.New("opened but failed")
			file, err := openJournalReaderWithFile(root, func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
				calls++
				var openErr error
				switch fault {
				case "different-file":
					owned, openErr = root.OpenFile("other", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
				case "directory":
					owned, openErr = root.Open(".")
				default:
					owned, openErr = boundOpenFile(root, name, flags, mode)
				}
				if openErr != nil {
					return owned, openErr
				}
				switch fault {
				case "closed-file":
					if err := owned.Close(); err != nil {
						t.Fatal(err)
					}
				case "closed-root":
					if err := root.Close(); err != nil {
						t.Fatal(err)
					}
				case "file-and-error":
					return owned, cause
				case "post-open-replacement":
					renameErr := os.Rename(filepath.Join(root.Name(), name), filepath.Join(root.Name(), "journal.held"))
					if runtime.GOOS == "windows" {
						if !errors.Is(renameErr, syscall.Errno(32)) {
							t.Fatal("actual opened reader must denyDELETE:", renameErr)
						}
						denied = true
					} else {
						if renameErr != nil {
							t.Fatal(renameErr)
						}
						if err := root.WriteFile(name, []byte("replacement"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				return owned, nil
			})
			if calls != 1 || owned == nil {
				t.Fatal("fault did not exercise real leaf open")
			}
			if denied {
				if err != nil || file == nil {
					t.Fatal("denied replacement lost valid reader:", err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				if file != nil || err == nil {
					t.Fatal("failed validation returned usable file")
				}
				switch fault {
				case "different-file", "directory", "post-open-replacement":
					boundStoreCode(t, normalizeBoundError(err), product.CodeInvalidArgument)
				case "closed-file":
					if !errors.Is(err, os.ErrClosed) {
						t.Fatal("cleanup lost second Close cause")
					}
				case "file-and-error":
					if !errors.Is(err, cause) {
						t.Fatal("cleanup lost opener cause")
					}
				}
			}
			boundStoreFileClosed(t, owned)
			if fault != "closed-root" {
				if _, err := root.Stat("."); err != nil {
					t.Fatal("leaf validation closed borrowed Root")
				}
			}
		})
	}
}

func TestOpenBoundReadonlyAndMissingHaveNoWriterEffects(t *testing.T) {
	for _, exists := range []bool{true, false} {
		t.Run(map[bool]string{true: "existing", false: "missing"}[exists], func(t *testing.T) {
			state := t.TempDir()
			if exists {
				s, err := Open("bound", state, store.Header{}, Options{})
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(state, "sessions", "bound", "writer.lock")); err != nil {
					t.Fatal(err)
				}
			}
			roots := boundStoreRoots(t, state, "bound", true)
			if err := roots.Namespace.Chmod(".", 0755); err != nil {
				t.Fatal(err)
			}
			if err := roots.Resource.Chmod(".", 0755); err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, state)
			calls := 0
			s, err := OpenBound("bound", roots, nil, store.Header{}, Options{ReadOnly: true,
				Write:    func(*os.File, []byte) (int, error) { calls++; return 0, os.ErrPermission },
				SyncFile: func(*os.File) error { calls++; return os.ErrPermission },
				SyncDir:  func(string) error { calls++; return os.ErrPermission },
			})
			if exists {
				if err != nil || s == nil || s.lock != nil {
					t.Fatal("readonly bound open did not remain lock-free:", err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				boundStoreCode(t, err, product.CodeNotFound)
				if s != nil {
					t.Fatal("readonly created missing journal")
				}
				boundStoreRootsLive(t, roots)
			}
			if calls != 0 || !reflect.DeepEqual(before, fileBoundaryTree(t, state)) {
				t.Fatal("readonly performed write, sync, chmod, lock or creation")
			}
		})
	}
}

func TestOpenBoundConstructionFaultsPreserveContextAndCallerOwnership(t *testing.T) {
	for _, fault := range []string{"closed-namespace", "closed-inspection", "final-open", "canceled-open", "deadline-open"} {
		t.Run(fault, func(t *testing.T) {
			state, dir := fileBoundaryJournal(t)
			roots := boundStoreRoots(t, state, "boundary", false)
			inspected, err := OpenJournalReader(roots.Resource)
			if err != nil {
				t.Fatal(err)
			}
			defer inspected.Close()
			if fault == "closed-namespace" {
				if err := roots.Namespace.Close(); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			var newFile *os.File
			s, err := openBoundWithFile("boundary", roots, inspected, store.Header{}, Options{OpenExisting: true}, func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
				calls++
				if fault == "closed-inspection" {
					if err := inspected.Close(); err != nil {
						t.Fatal(err)
					}
				} else if fault == "final-open" {
					return nil, errors.New("private path must not leak")
				} else if fault == "canceled-open" {
					return nil, context.Canceled
				} else if fault == "deadline-open" {
					return nil, context.DeadlineExceeded
				}
				var openErr error
				newFile, openErr = boundOpenFile(root, name, flags, mode)
				return newFile, openErr
			})
			if s != nil {
				t.Fatal("constructor fault returned store")
			}
			if fault == "canceled-open" || fault == "deadline-open" {
				want := context.Canceled
				if fault == "deadline-open" {
					want = context.DeadlineExceeded
				}
				if !errors.Is(normalizeBoundError(err), want) {
					t.Fatal("constructor swallowed caller context")
				}
			} else {
				boundStoreCode(t, normalizeBoundError(err), product.CodeStorageUnavailable)
			}
			wantCalls := 1
			if fault == "closed-namespace" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("actual final calls=%d want=%d", calls, wantCalls)
			}
			if newFile != nil {
				boundStoreFileClosed(t, newFile)
			}
			if _, err := roots.Resource.Stat("."); err != nil {
				t.Fatal("constructor failure closed caller Resource root")
			}
			if fault != "closed-namespace" {
				boundStoreRootsLive(t, roots)
			}
			if fault != "closed-inspection" {
				if _, err := inspected.Stat(); err != nil {
					t.Fatal("constructor failure closed caller inspection")
				}
			}
			boundLockProbe(t, dir, "legacy-available")
		})
	}
}

func TestBoundLeafFiniteOpenRejectsUnsafeFlags(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, tc := range []struct {
		name  string
		flags int
	}{{"other", os.O_RDONLY}, {"journal.jsonl", os.O_TRUNC | os.O_RDWR}, {"writer.lock", os.O_CREATE | os.O_RDONLY}} {
		file, err := boundOpenFile(root, tc.name, tc.flags, 0600)
		boundStoreCode(t, err, product.CodeInvalidArgument)
		if file != nil {
			t.Fatal("invalid finite open returned file")
		}
	}
	directory, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid flags created leaves")
	}
}

func TestOpenBoundCallerInspectionMustMatchNamedJournal(t *testing.T) {
	state, dir := fileBoundaryJournal(t)
	roots := boundStoreRoots(t, state, "boundary", false)
	other, err := os.CreateTemp(t.TempDir(), "other-journal")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	raw, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Write(raw); err != nil {
		t.Fatal(err)
	}
	before := fileBoundaryTree(t, state)
	s, err := OpenBound("boundary", roots, other, store.Header{}, Options{OpenExisting: true})
	boundStoreCode(t, err, product.CodeInvalidArgument)
	if s != nil || !reflect.DeepEqual(before, fileBoundaryTree(t, state)) {
		t.Fatal("foreign inspection caused writer side effects")
	}
	if _, err := other.Stat(); err != nil {
		t.Fatal("foreign caller inspection was closed")
	}
	boundStoreRootsLive(t, roots)
	after, err := os.ReadFile(other.Name())
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatal("rejection changed caller inspection")
	}
}
