//go:build windows

package jsonl

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"golang.org/x/sys/windows"
)

func TestWriterLockWindowsStoreUnlockFailureClosesEveryOwner(t *testing.T) {
	for _, fault := range []string{"already-unlocked", "journal-and-lock-closed"} {
		t.Run(fault, func(t *testing.T) {
			state := t.TempDir()
			s, err := Open("close", state, store.Header{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			file, journal, roots := s.lock.file, s.journal, s.roots
			lockPath := filepath.Join(state, "sessions", "close", "writer.lock")
			if fault == "already-unlocked" {
				if err := unlockWriterFile(file); err != nil {
					t.Fatal("actual initial unlock:", err)
				}
			} else {
				if err := journal.Close(); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			closeErr := s.Close() // Default production unlock; File.Close must still run.
			boundStoreCode(t, closeErr, product.CodeStorageUnavailable)
			if pe, _ := product.AsError(closeErr); pe.Message != "session store close failed" {
				t.Fatal("public close message contains a raw cause")
			}
			if fault == "already-unlocked" {
				if !errors.Is(s.closeCause, windows.ERROR_NOT_LOCKED) {
					t.Fatal("lost actual default UnlockFileEx failure:", s.closeCause)
				}
			} else if !errors.Is(s.closeCause, windows.ERROR_INVALID_HANDLE) || !errors.Is(s.closeCause, os.ErrClosed) {
				t.Fatal("lost real unlock and File.Close causes:", s.closeCause)
			}
			boundStoreFileClosed(t, file)
			boundStoreFileClosed(t, journal)
			for _, root := range []*os.Root{roots.Resource, roots.Namespace} {
				if _, err := root.Stat("."); err == nil {
					t.Fatal("unlock error prevented owned roots closing")
				}
			}
			if s.lock != nil || s.journal != nil || s.roots != nil || s.ResourceRoot() != nil {
				t.Fatal("closed store retained owned values")
			}
			boundLockProbeFile(t, lockPath, "legacy-available")
			step12NativeProbe(t, lockPath, "leaf-available")
			if err := s.Close(); err != nil {
				t.Fatal("repeated Store.Close:", err)
			}
		})
	}
}

func TestWriterLockWindowsWrongRangeFailureStillCloses(t *testing.T) {
	path := t.TempDir()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := openWriterLock(root)
	if err != nil {
		t.Fatal(err)
	}
	file := lock.file
	boundLockProbe(t, path, "legacy-busy")
	err = lock.closeWithUnlock(func(file *os.File) error {
		return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{Offset: 1})
	})
	if !errors.Is(err, windows.ERROR_NOT_LOCKED) {
		t.Fatal("actual wrong-range unlock did not fail:", err)
	}
	boundStoreFileClosed(t, file)
	boundLockProbe(t, path, "legacy-available")
	step12NativeProbe(t, filepath.Join(path, "writer.lock"), "leaf-available")
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriterLockWindowsKeepsOriginalRightsAndReadonlyAttribute(t *testing.T) {
	state, path := fileBoundaryJournal(t)
	roots := boundStoreRoots(t, state, "boundary", false)
	reader, err := OpenJournalReader(roots.Resource)
	if err != nil {
		t.Fatal(err)
	}
	if reader.Name() != "journal.jsonl" || step13NativeAccess(t, reader) != windows.FILE_GENERIC_READ {
		t.Fatal("reader native rights or basename changed")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenBound("boundary", roots, nil, store.Header{}, Options{OpenExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	if step13NativeAccess(t, s.journal) != windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE || step13NativeAccess(t, s.lock.file) != windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE || s.lock.file.Name() != "writer.lock" {
		t.Fatal("default existing writer or lock lost original READ|WRITE rights/name")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, leaf := range []string{"journal.jsonl", "writer.lock"} {
		t.Run(leaf, func(t *testing.T) {
			name := filepath.Join(path, leaf)
			if err := os.Chmod(name, 0400); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(name, 0600)
			before := step13NativeDirectoryEntryAttrs(t, name)
			legacy, oldErr := os.OpenFile(name, os.O_CREATE|os.O_RDONLY, 0600)
			if legacy != nil {
				_ = legacy.Close()
			}
			if !errors.Is(oldErr, windows.ERROR_ACCESS_DENIED) {
				t.Fatal("original lock baseline did not require WRITE:", oldErr)
			}
			roots := boundStoreRoots(t, state, "boundary", false)
			failed, err := OpenBound("boundary", roots, nil, store.Header{}, Options{OpenExisting: true})
			boundStoreCode(t, err, product.CodeStorageUnavailable)
			if failed != nil || before != step13NativeDirectoryEntryAttrs(t, name) {
				t.Fatal("writer cleared readonly to bypass original WRITE permission")
			}
			boundStoreRootsLive(t, roots)
			reader, err := OpenJournalReader(roots.Resource)
			if err != nil {
				t.Fatal("readonly journal must remain readable:", err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func step13NativeDirectoryEntryAttrs(t *testing.T, path string) uint32 {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := windows.GetFileAttributes(name)
	if err != nil {
		t.Fatal(err)
	}
	return attrs
}
