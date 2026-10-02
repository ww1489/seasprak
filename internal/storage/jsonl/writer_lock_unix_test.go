//go:build !windows

package jsonl

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"golang.org/x/sys/unix"
)

func TestWriterLockUnixExistingReadonlyAcceptance(t *testing.T) {
	path := t.TempDir()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("writer.lock", []byte("readonly lock"), 0400); err != nil {
		t.Fatal(err)
	}
	lock, err := openWriterLock(root)
	if err != nil {
		t.Fatal("default Unix Flock must accept the unchanged readonly lock:", err)
	}
	defer lock.Close()
	flags, err := unix.FcntlInt(lock.file.Fd(), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_ACCMODE != unix.O_RDONLY {
		t.Fatal("default Unix lock did not actually retain O_RDONLY")
	}
	info, err := lock.file.Stat()
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatal("lock acquisition changed readonly mode")
	}
	boundLockProbe(t, path, "legacy-busy")
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	boundLockProbe(t, path, "legacy-available")
}

func TestWriterLockUnixStoreCloseAttemptsAllOwnersAfterRealErrors(t *testing.T) {
	state := t.TempDir()
	s, err := Open("close", state, store.Header{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	file, journal, roots := s.lock.file, s.journal, s.roots
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	boundStoreCode(t, s.Close(), product.CodeStorageUnavailable)
	if !errors.Is(s.closeCause, unix.EBADF) || !errors.Is(s.closeCause, os.ErrClosed) {
		t.Fatal("Store.Close lost real unlock and file-close causes:", s.closeCause)
	}
	boundStoreFileClosed(t, file)
	boundStoreFileClosed(t, journal)
	for _, root := range []*os.Root{roots.Resource, roots.Namespace} {
		if _, err := root.Stat("."); err == nil {
			t.Fatal("File.Close errors prevented remaining Root.Close")
		}
	}
	boundLockProbe(t, filepath.Join(state, "sessions", "close"), "legacy-available")
	if s.ResourceRoot() != nil || s.lock != nil || s.journal != nil || s.roots != nil {
		t.Fatal("failed close retained owned values")
	}
	if err := s.Close(); err != nil {
		t.Fatal("repeat Store.Close:", err)
	}
}

func TestWriterLockUnixReadonlyAndSingleReopenContract(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        os.FileMode
		first       error
		second      error
		busy        bool
		wantCalls   int
		wantOpens   int
		wantSuccess bool
	}{
		{"eio-reopen", 0600, unix.EIO, nil, false, 2, 2, true},
		{"ebadf-reopen", 0600, unix.EBADF, nil, false, 2, 2, true},
		{"readonly-mode-no-reopen", 0400, unix.EIO, nil, false, 1, 1, false},
		{"ebadf-readonly-no-reopen", 0400, unix.EBADF, nil, false, 1, 1, false},
		{"other-error-no-reopen", 0600, unix.EINVAL, nil, false, 1, 1, false},
		{"second-eio-no-third", 0600, unix.EIO, unix.EIO, false, 2, 2, false},
		{"second-ebadf-no-third", 0600, unix.EBADF, unix.EBADF, false, 2, 2, false},
		{"second-busy", 0600, unix.EIO, nil, true, 2, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir()
			root, err := os.OpenRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := os.WriteFile(filepath.Join(path, "writer.lock"), []byte("lock sentinel"), tc.mode); err != nil {
				t.Fatal(err)
			}
			calls, opens := 0, 0
			var files []*os.File
			lk, lockErr := openWriterLockWith(root, func(root *os.Root, name string, flags int, perm os.FileMode) (*os.File, error) {
				opens++
				if name != "writer.lock" || flags&os.O_CREATE != 0 {
					t.Fatal("existing retry changed finite leaf or creation semantics")
				}
				want := os.O_RDONLY
				if opens == 2 {
					want = os.O_RDWR
				}
				if flags != want {
					t.Fatalf("open %d flags=%d want=%d", opens, flags, want)
				}
				file, err := boundOpenFile(root, name, flags, perm)
				if file != nil {
					files = append(files, file)
				}
				return file, err
			}, func(file *os.File) (bool, error) {
				calls++
				actual, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
				if err != nil {
					t.Fatal(err)
				}
				if calls == 1 {
					if actual&unix.O_ACCMODE != unix.O_RDONLY {
						t.Fatal("Unix initial lock acquired WRITE unexpectedly")
					}
					return false, tc.first
				}
				if actual&unix.O_ACCMODE != unix.O_RDWR {
					t.Fatal("eligible retry did not actually reopen read-write")
				}
				if tc.busy || tc.second != nil {
					return false, tc.second
				}
				return tryWriterFileLock(file)
			})
			if calls != tc.wantCalls || opens != tc.wantOpens || (lk != nil) != tc.wantSuccess {
				t.Fatalf("calls=%d opens=%d owner=%v want calls=%d opens=%d success=%v error=%v", calls, opens, lk != nil, tc.wantCalls, tc.wantOpens, tc.wantSuccess, lockErr)
			}
			if tc.wantSuccess {
				if lockErr != nil {
					t.Fatal(lockErr)
				}
				if err := lk.Close(); err != nil {
					t.Fatal(err)
				}
			} else if tc.busy {
				boundStoreCode(t, normalizeBoundError(lockErr), product.CodeStateConflict)
			} else if !errors.Is(lockErr, tc.first) && !errors.Is(lockErr, tc.second) {
				t.Fatal("lost actual injected system error:", lockErr)
			}
			for _, file := range files {
				boundStoreFileClosed(t, file)
			}
			legacy := flock.New(filepath.Join(path, "writer.lock"))
			if ok, err := legacy.TryLock(); err != nil || !ok {
				t.Fatal("retry path retained writer")
			}
			if err := legacy.Unlock(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWriterLockUnixRetryIdentityAndCloseFailures(t *testing.T) {
	for _, fault := range []string{"replacement", "old-close", "new-open", "closed-fd"} {
		t.Run(fault, func(t *testing.T) {
			path := t.TempDir()
			root, err := os.OpenRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			var files []*os.File
			calls, opens := 0, 0
			lk, lockErr := openWriterLockWith(root, func(root *os.Root, name string, flags int, perm os.FileMode) (*os.File, error) {
				opens++
				if opens == 2 && fault == "new-open" {
					return nil, unix.EACCES
				}
				f, err := boundOpenFile(root, name, flags, perm)
				if f != nil {
					files = append(files, f)
				}
				if opens == 2 && fault == "old-close" {
					if err := files[0].Close(); err != nil {
						t.Fatal(err)
					}
				}
				return f, err
			}, func(file *os.File) (bool, error) {
				calls++
				if calls > 1 {
					t.Fatal("failure proceeded to second Flock")
				}
				if fault == "replacement" {
					if err := root.Rename("writer.lock", "writer.held"); err != nil {
						t.Fatal(err)
					}
					other, err := root.OpenFile("writer.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
					if err != nil {
						t.Fatal(err)
					}
					if err := other.Close(); err != nil {
						t.Fatal(err)
					}
				} else if fault == "closed-fd" {
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
					return tryWriterFileLock(file) // Actual EBADF, not a synthetic one.
				}
				return false, unix.EIO // Per-call synthetic NFS branch; no NFS claim.
			})
			if lk != nil || lockErr == nil || calls != 1 {
				t.Fatal("retry failure returned usable owner")
			}
			wantOpens := 2
			if fault == "closed-fd" {
				wantOpens = 1
			}
			if opens != wantOpens {
				t.Fatalf("opens=%d want=%d", opens, wantOpens)
			}
			if fault == "replacement" {
				boundStoreCode(t, normalizeBoundError(lockErr), product.CodeInvalidArgument)
			} else if fault == "old-close" || fault == "closed-fd" {
				if !errors.Is(lockErr, os.ErrClosed) {
					t.Fatal("lost real Close failure:", lockErr)
				}
			} else if !errors.Is(lockErr, unix.EACCES) {
				t.Fatal("lost retry open failure")
			}
			for _, file := range files {
				boundStoreFileClosed(t, file)
			}
			if _, err := root.Stat("."); err != nil {
				t.Fatal("retry failure closed borrowed Root")
			}
		})
	}
}

func TestWriterLockUnixUnlockFailureStillClosesFile(t *testing.T) {
	for _, fault := range []string{"synthetic-unlock", "real-closed-fd"} {
		t.Run(fault, func(t *testing.T) {
			path := t.TempDir()
			root, err := os.OpenRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			lk, err := openWriterLock(root)
			if err != nil {
				t.Fatal(err)
			}
			file := lk.file
			if fault == "real-closed-fd" {
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				err = lk.Close()
				if !errors.Is(err, unix.EBADF) || !errors.Is(err, os.ErrClosed) {
					t.Fatal("real Unlock and File.Close reasons were not both retained:", err)
				}
			} else {
				err = lk.closeWithUnlock(func(*os.File) error { return unix.EIO })
				if !errors.Is(err, unix.EIO) {
					t.Fatal("lost synthetic unlock error")
				}
			}
			boundStoreFileClosed(t, file)
			if err := lk.Close(); err != nil {
				t.Fatal("repeated close:", err)
			}
			legacy := flock.New(filepath.Join(path, "writer.lock"))
			if ok, err := legacy.TryLock(); err != nil || !ok {
				t.Fatal("Unlock failure retained private writer FD")
			}
			if err := legacy.Unlock(); err != nil {
				t.Fatal(err)
			}
			if err := root.Rename("writer.lock", "released.lock"); err != nil {
				t.Fatal("close did not release leaf:", err)
			}
		})
	}
}
