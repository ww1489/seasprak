package jsonl

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestWriterLockBoundInteropAndNonblocking(t *testing.T) {
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
	defer lock.Close()
	boundLockProbe(t, path, "legacy-busy")
	boundLockProbe(t, path, "bound-busy")
	calls := 0
	second, err := openWriterLockWith(root, boundOpenFile, func(f *os.File) (bool, error) { calls++; return tryWriterFileLock(f) })
	boundStoreCode(t, normalizeBoundError(err), product.CodeStateConflict)
	if second != nil || calls != 1 {
		t.Fatal("busy lock retried or returned owner")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	boundLockProbe(t, path, "legacy-available")
	boundLockProbe(t, path, "bound-available")
	legacy := flock.New(filepath.Join(path, "writer.lock"))
	if ok, err := legacy.TryLock(); err != nil || !ok {
		t.Fatal("legacy acquire:", err)
	}
	defer legacy.Unlock()
	boundLockProbe(t, path, "bound-busy")
	if err := legacy.Unlock(); err != nil {
		t.Fatal(err)
	}
	boundLockProbe(t, path, "bound-available")
}

func TestWriterLockBoundNewCollisionAndPostLockIdentity(t *testing.T) {
	for _, fault := range []string{"collision", "post-lock-replacement"} {
		t.Run(fault, func(t *testing.T) {
			path := t.TempDir()
			root, err := os.OpenRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			var file *os.File
			opens, tries, denied := 0, 0, false
			lk, lockErr := openWriterLockWith(root, func(root *os.Root, name string, flags int, perm os.FileMode) (*os.File, error) {
				opens++
				if flags != os.O_RDONLY|os.O_CREATE|os.O_EXCL || name != "writer.lock" {
					t.Fatal("new private lock did not preserve readonly/exclusive finite flags")
				}
				if fault == "collision" {
					collision, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := collision.WriteString("collision sentinel"); err != nil {
						t.Fatal(err)
					}
					if err := collision.Close(); err != nil {
						t.Fatal(err)
					}
				}
				file, err = boundOpenFile(root, name, flags, perm)
				return file, err
			}, func(file *os.File) (bool, error) {
				tries++
				ok, err := tryWriterFileLock(file)
				if err != nil || !ok {
					return ok, err
				}
				renameErr := os.Rename(filepath.Join(path, "writer.lock"), filepath.Join(path, "writer.held"))
				if runtime.GOOS == "windows" {
					if !errors.Is(renameErr, syscall.Errno(32)) {
						t.Fatal("actual locked native leaf did not denyDELETE:", renameErr)
					}
					denied = true
				} else {
					if renameErr != nil {
						t.Fatal(renameErr)
					}
					other, err := root.OpenFile("writer.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
					if err != nil {
						t.Fatal(err)
					}
					if err := other.Close(); err != nil {
						t.Fatal(err)
					}
				}
				return true, nil
			})
			if opens != 1 {
				t.Fatal("private lock used additional open")
			}
			if fault == "collision" {
				boundStoreCode(t, normalizeBoundError(lockErr), product.CodeStorageUnavailable)
				if tries != 0 || lk != nil {
					t.Fatal("lock collision proceeded to locking")
				}
				raw, _ := root.ReadFile("writer.lock")
				if string(raw) != "collision sentinel" {
					t.Fatal("collision changed existing lock bytes")
				}
			} else if runtime.GOOS == "windows" {
				if !denied || tries != 1 || lockErr != nil || lk == nil {
					t.Fatal("OS-denied replacement did not retain original owner:", lockErr)
				}
				if err := lk.Close(); err != nil {
					t.Fatal(err)
				}
				boundStoreFileClosed(t, file)
			} else {
				boundStoreCode(t, normalizeBoundError(lockErr), product.CodeInvalidArgument)
				if lk != nil || tries != 1 {
					t.Fatal("post-lock replacement returned usable owner")
				}
				boundStoreFileClosed(t, file)
				boundLockProbeFile(t, filepath.Join(path, "writer.held"), "legacy-available")
			}
			boundLockProbe(t, path, "legacy-available")
		})
	}
}

func TestWriterLockBoundSubprocess(t *testing.T) {
	if os.Getenv("SEASPRAK_BOUND_LOCK_HELPER") != "1" {
		return
	}
	path, action := os.Getenv("SEASPRAK_BOUND_LOCK_FILE"), os.Getenv("SEASPRAK_BOUND_LOCK_ACTION")
	available := action == "legacy-available" || action == "bound-available"
	if action == "legacy-busy" || action == "legacy-available" {
		lock := flock.New(path)
		ok, err := lock.TryLock()
		if err != nil || ok != available {
			t.Fatalf("legacy acquired=%v want=%v error=%v", ok, available, err)
		}
		if err := lock.Unlock(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if action != "bound-busy" && action != "bound-available" {
		t.Fatal("invalid helper action")
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := openWriterLock(root)
	if available {
		if err != nil || lock == nil {
			t.Fatal("production private lock not available:", err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		boundStoreCode(t, normalizeBoundError(err), product.CodeStateConflict)
		if lock != nil {
			_ = lock.Close()
			t.Fatal("production private lock unexpectedly acquired")
		}
	}
}

func boundLockProbe(t *testing.T, dir, action string) {
	t.Helper()
	boundLockProbeFile(t, filepath.Join(dir, "writer.lock"), action)
}

func boundLockProbeFile(t *testing.T, path, action string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWriterLockBoundSubprocess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SEASPRAK_BOUND_LOCK_HELPER=1", "SEASPRAK_BOUND_LOCK_FILE="+path, "SEASPRAK_BOUND_LOCK_ACTION="+action)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("real subprocess %s: %v: %s", action, err, out)
	}
}
