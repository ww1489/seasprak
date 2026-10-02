//go:build windows

package jsonl

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/gofrs/flock"
	"golang.org/x/sys/windows"
)

// This follow-up is test-only: no Store, Repair, or production lock is changed.
var (
	step12NativeInvalid = errors.New("step12 native lock: invalid leaf or identity")
	step12NativeBusy    = errors.New("step12 native lock: busy")
)

// The only name is the fixed ordinary leaf writer.lock. The caller borrows both
// directory objects; the returned file owns only its own native handle.
func step12NativeOpen(directory *os.File, create bool) (*os.File, error) {
	name, err := windows.NewNTUnicodeString("writer.lock")
	if err != nil {
		return nil, err
	}
	attrs := &windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(directory.Fd()), ObjectName: name, Attributes: windows.OBJ_DONT_REPARSE}
	attrs.Length = uint32(unsafe.Sizeof(*attrs))
	disposition := uint32(windows.FILE_OPEN)
	if create {
		disposition = windows.FILE_CREATE // Exclusive creation, never OPEN_IF.
	}
	var h windows.Handle
	err = windows.NtCreateFile(&h, windows.FILE_GENERIC_READ, attrs, &windows.IO_STATUS_BLOCK{}, nil, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, disposition,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(h), "step12-relative-writer.lock")
	if file == nil {
		return nil, errors.Join(errors.New("wrap native lock handle"), windows.CloseHandle(h))
	}
	return file, nil
}

func step12NativeRegular(info os.FileInfo) bool {
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && info.Mode().IsRegular() && attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0
}

func step12NativeAcquire(root *os.Root, directory *os.File, beforeOpen func()) (*os.File, error) {
	bound, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	openedDir, err := directory.Stat()
	if err != nil || !os.SameFile(bound, openedDir) {
		return nil, errors.Join(step12NativeInvalid, err)
	}
	before, err := root.Lstat("writer.lock")
	create := errors.Is(err, os.ErrNotExist)
	if err != nil && !create {
		return nil, err
	}
	if !create && !step12NativeRegular(before) {
		return nil, step12NativeInvalid
	}
	if beforeOpen != nil {
		beforeOpen() // Per-call test barrier, not a global or production hook.
	}
	file, err := step12NativeOpen(directory, create)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) { return nil, errors.Join(err, file.Close()) }
	opened, err := file.Stat()
	if err != nil || !step12NativeRegular(opened) || (!create && !os.SameFile(before, opened)) {
		return fail(errors.Join(step12NativeInvalid, err))
	}
	current, err := root.Lstat("writer.lock")
	if err != nil || !step12NativeRegular(current) || !os.SameFile(opened, current) {
		return fail(errors.Join(step12NativeInvalid, err))
	}
	ok, err := fileBoundaryTryHandleLock(file)
	if err != nil {
		return fail(err)
	}
	if !ok {
		return fail(step12NativeBusy)
	}
	current, err = root.Lstat("writer.lock")
	if err != nil || !step12NativeRegular(current) || !os.SameFile(opened, current) {
		return nil, errors.Join(step12NativeInvalid, err, step12NativeClose(file, fileBoundaryUnlockHandle))
	}
	return file, nil
}

// An unlock failure must still close the file and preserve both errors.
func step12NativeClose(file *os.File, unlock func(*os.File) error) error {
	unlockErr := unlock(file)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}

func step12NativeFixture(t *testing.T) (*os.Root, *os.File, string) {
	t.Helper()
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "session"), 0700); err != nil {
		t.Fatal(err)
	}
	trusted, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = trusted.Close() })
	root, err := trusted.OpenRoot("session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	directory, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	return root, directory, filepath.Join(state, "session")
}

func TestFileBoundaryNativeLockFollowupInterop(t *testing.T) {
	root, directory, path := step12NativeFixture(t)
	file, err := step12NativeAcquire(root, directory, nil)
	if err != nil {
		t.Fatal("exclusive-create read-only lock:", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	// Write must fail even though exclusive byte-range locking works.
	if n, err := file.Write([]byte("forbidden")); n != 0 || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("read-only handle write n=%d error=%v", n, err)
	}
	lockPath := filepath.Join(path, "writer.lock")
	step12NativeProbe(t, lockPath, "legacy-busy")
	step12NativeProbe(t, lockPath, "native-busy")
	step12NativeProbe(t, lockPath, "leaf-blocked")
	if err := step12NativeClose(file, fileBoundaryUnlockHandle); err != nil {
		t.Fatal(err)
	}
	step12NativeProbe(t, lockPath, "legacy-available")
	step12NativeProbe(t, lockPath, "leaf-available")

	legacy := flock.New(lockPath)
	ok, err := legacy.TryLock()
	if err != nil || !ok {
		t.Fatal("legacy lock acquisition:", ok, err)
	}
	step12NativeProbe(t, lockPath, "native-busy")
	if err := legacy.Unlock(); err != nil {
		t.Fatal(err)
	}
	file, err = step12NativeAcquire(root, directory, nil)
	if err != nil {
		t.Fatal("reopen existing read-only lock:", err)
	}
	if err := file.Close(); err != nil { // Close alone must release the lock.
		t.Fatal(err)
	}
	step12NativeProbe(t, lockPath, "legacy-available")
	step12NativeProbe(t, lockPath, "native-available")
	if _, err := fileBoundaryTryHandleLock(file); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		t.Fatalf("closed handle lock=%v", err)
	}
	t.Log("FILE_GENERIC_READ locks successfully; write denied; bidirectional legacy/native process exclusion; close releases; held leaf rename/delete blocked")
}

func TestFileBoundaryNativeLockFollowupFaultClose(t *testing.T) {
	root, directory, path := step12NativeFixture(t)
	file, err := step12NativeAcquire(root, directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	calls := 0
	err = step12NativeClose(file, func(file *os.File) error {
		calls++
		// A real OS failure: unlock offset 1 although only offset 0 is locked.
		return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{Offset: 1})
	})
	if calls != 1 || !errors.Is(err, windows.ERROR_NOT_LOCKED) {
		t.Fatalf("actual unlock fault: calls=%d error=%v", calls, err)
	}
	if _, err := file.Stat(); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		t.Fatalf("unlock failure did not invalidate native handle: %v", err)
	}
	step12NativeProbe(t, filepath.Join(path, "writer.lock"), "legacy-available")
	step12NativeProbe(t, filepath.Join(path, "writer.lock"), "leaf-available")
	// Both invalid-handle unlock and already-closed file errors are retained.
	err = step12NativeClose(file, fileBoundaryUnlockHandle)
	if !errors.Is(err, windows.ERROR_INVALID_HANDLE) || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("cleanup error aggregation=%v", err)
	}
	t.Log("real wrong-range unlock error retained; Close still releases; second cleanup retains both errors")
}

func TestFileBoundaryNativeLockFollowupRejectsLeaves(t *testing.T) {
	for _, kind := range []string{"directory", "junction", "file-symlink", "identity-replacement", "exclusive-create-collision", "closed-directory"} {
		t.Run(kind, func(t *testing.T) {
			root, directory, path := step12NativeFixture(t)
			outside := t.TempDir()
			external := filepath.Join(outside, "outside.lock")
			if err := os.WriteFile(external, []byte("external lock sentinel"), 0644); err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(path, "writer.lock")
			var barrier func()
			switch kind {
			case "directory":
				if err := os.Mkdir(lockPath, 0700); err != nil {
					t.Fatal(err)
				}
			case "junction":
				fileBoundaryDirectoryLink(t, lockPath, outside)
			case "file-symlink":
				fileBoundaryFileLink(t, lockPath, external)
			case "identity-replacement":
				if err := os.WriteFile(lockPath, []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
				barrier = func() {
					if err := os.Rename(lockPath, lockPath+".held"); err != nil {
						t.Fatal(err)
					}
					if err := os.Link(external, lockPath); err != nil {
						t.Fatal(err)
					}
				}
			case "exclusive-create-collision":
				barrier = func() {
					if err := os.WriteFile(lockPath, []byte("collision sentinel"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "closed-directory":
				barrier = func() {
					if err := directory.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := fileBoundaryTree(t, outside)
			calls := 0
			file, err := step12NativeAcquire(root, directory, func() {
				calls++
				if barrier != nil {
					barrier()
				}
			})
			if file != nil {
				_ = step12NativeClose(file, fileBoundaryUnlockHandle)
				t.Fatal("rejected leaf returned a lock handle")
			}
			want := error(step12NativeInvalid)
			if kind == "exclusive-create-collision" {
				want = windows.STATUS_OBJECT_NAME_COLLISION
			} else if kind == "closed-directory" {
				// Closed File.Fd() is -1, a native pseudo-process handle; using it
				// as RootDirectory returns a type mismatch, not INVALID_HANDLE.
				want = windows.STATUS_OBJECT_TYPE_MISMATCH
			}
			if !errors.Is(err, want) {
				t.Fatalf("rejection error=%v, want %v", err, want)
			}
			wantCalls := 1
			if kind == "directory" || kind == "junction" || kind == "file-symlink" {
				wantCalls = 0
				// OPEN_REPARSE_POINT may open a file link itself; the checked
				// wrapper must reject it, and raw open must never follow its target.
				f, nativeErr := step12NativeOpen(directory, false)
				if f != nil {
					defer f.Close()
					if kind != "file-symlink" || nativeErr != nil {
						t.Fatal("native flags accepted an invalid directory leaf")
					}
					opened, statErr := f.Stat()
					if statErr != nil {
						t.Fatal(statErr)
					}
					attrs, ok := opened.Sys().(*syscall.Win32FileAttributeData)
					if !ok || attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0 || step12NativeRegular(opened) {
						t.Fatal("raw native link open lost its reparse type")
					}
					leaf, leafErr := root.Lstat("writer.lock")
					target, targetErr := os.Lstat(external)
					if leafErr != nil || targetErr != nil || !os.SameFile(opened, leaf) || os.SameFile(opened, target) {
						t.Fatal("raw native link open followed or changed its target")
					}
					if err := f.Close(); err != nil {
						t.Fatal(err)
					}
					t.Log("raw native flags open the reparse leaf; checked wrapper rejects it without locking the target")
				} else {
					if nativeErr == nil {
						t.Fatal("native flags returned neither a file nor a rejection")
					}
					t.Logf("native flags independently reject %s: %v", kind, nativeErr)
				}
			}
			if calls != wantCalls {
				t.Fatalf("per-call boundary calls=%d, want %d", calls, wantCalls)
			}
			if kind == "identity-replacement" || kind == "exclusive-create-collision" {
				// Rejection must close any opened native handle, not merely avoid locking.
				if err := os.Rename(lockPath, lockPath+".rejected"); err != nil {
					t.Fatal("rejected native handle leaked no-DELETE sharing:", err)
				}
				if kind == "exclusive-create-collision" {
					data, err := os.ReadFile(lockPath + ".rejected")
					if err != nil || string(data) != "collision sentinel" {
						t.Fatal("exclusive create overwrote collision", err)
					}
				}
			}
			step12NativeProbe(t, external, "legacy-available")
			if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
				t.Error("rejected lock changed external bytes, modes or entries")
			}
		})
	}
}

func TestFileBoundaryNativeLockFollowupBoundDirectory(t *testing.T) {
	for _, kind := range []string{"new", "existing"} {
		t.Run(kind, func(t *testing.T) {
			root, directory, path := step12NativeFixture(t)
			if kind == "existing" {
				if err := os.WriteFile(filepath.Join(path, "writer.lock"), []byte("original lock"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "writer.lock"), []byte("external lock sentinel"), 0644); err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, outside)
			if err := os.Rename(path, path+".held"); err != nil {
				t.Fatal("retained Root/directory must allow pre-lock directory rename:", err)
			}
			fileBoundaryDirectoryLink(t, path, outside)
			file, err := step12NativeAcquire(root, directory, nil)
			if err != nil {
				t.Fatal("native relative open after directory replacement:", err)
			}
			t.Cleanup(func() { _ = file.Close() })
			step12NativeProbe(t, filepath.Join(path+".held", "writer.lock"), "legacy-busy")
			step12NativeProbe(t, filepath.Join(outside, "writer.lock"), "legacy-available")
			if err := step12NativeClose(file, fileBoundaryUnlockHandle); err != nil {
				t.Fatal(err)
			}
			step12NativeProbe(t, filepath.Join(path+".held", "writer.lock"), "legacy-available")
			if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
				t.Error("directory replacement touched outside")
			}
			t.Log("Root/directory handles share DELETE before lock; relative open locks original renamed directory; outside bytes/modes/entries and lock remain untouched")
		})
	}
}

func TestFileBoundaryNativeLockFollowupParentSharing(t *testing.T) {
	// Compare the actual old flock and native lock under the same retained Root
	// handles. Parent-directory behavior is observed, not inferred from leaf flags.
	var legacyBlocked bool
	for _, method := range []string{"legacy", "native"} {
		t.Run(method, func(t *testing.T) {
			root, directory, path := step12NativeFixture(t)
			var closeLock func() error
			if method == "legacy" {
				lock := flock.New(filepath.Join(path, "writer.lock"))
				ok, err := lock.TryLock()
				if err != nil || !ok {
					t.Fatal(ok, err)
				}
				closeLock = lock.Unlock
			} else {
				file, err := step12NativeAcquire(root, directory, nil)
				if err != nil {
					t.Fatal(err)
				}
				closeLock = func() error { return step12NativeClose(file, fileBoundaryUnlockHandle) }
			}
			t.Cleanup(func() { _ = closeLock() })
			err := os.Rename(path, path+".held")
			blocked := err != nil
			if blocked && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				t.Fatal("unexpected parent rename error:", err)
			}
			if method == "legacy" {
				legacyBlocked = blocked
			} else if blocked != legacyBlocked {
				t.Errorf("parent rename blocked=%v, legacy blocked=%v", blocked, legacyBlocked)
			}
			t.Logf("%s held-lock parent rename blocked=%v error=%v", method, blocked, err)
			actual := path
			if !blocked {
				actual += ".held"
			}
			step12NativeProbe(t, filepath.Join(actual, "writer.lock"), "legacy-busy")
			parentAction := "parent-available"
			if blocked {
				parentAction = "parent-blocked"
			}
			step12NativeProbe(t, actual, parentAction)
			if err := closeLock(); err != nil {
				t.Fatal(err)
			}
			step12NativeProbe(t, filepath.Join(actual, "writer.lock"), "legacy-available")
			step12NativeProbe(t, actual, "parent-available")
			if blocked {
				if err := os.Rename(path, path+".held"); err != nil {
					t.Fatal("Root/directory handles obstruct parent rename after close:", err)
				}
			}
		})
	}
}

func TestFileBoundaryNativeLockFollowupSubprocess(t *testing.T) {
	if os.Getenv("SEASPRAK_STEP12_NATIVE_LOCK_HELPER") != "1" {
		return
	}
	path, action := os.Getenv("SEASPRAK_STEP12_NATIVE_LOCK_PATH"), os.Getenv("SEASPRAK_STEP12_NATIVE_LOCK_ACTION")
	switch action {
	case "legacy-busy", "legacy-available":
		lock := flock.New(path)
		ok, err := lock.TryLock()
		if err != nil || ok != (action == "legacy-available") {
			t.Fatalf("legacy acquired=%v action=%s error=%v", ok, action, err)
		}
		if err := lock.Unlock(); err != nil {
			t.Fatal(err)
		}
	case "native-busy", "native-available":
		root, err := os.OpenRoot(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		directory, err := root.Open(".")
		if err != nil {
			t.Fatal(err)
		}
		defer directory.Close()
		file, err := step12NativeAcquire(root, directory, nil)
		if action == "native-busy" {
			if file != nil || !errors.Is(err, step12NativeBusy) {
				if file != nil {
					_ = step12NativeClose(file, fileBoundaryUnlockHandle)
				}
				t.Fatal("native expected busy:", err)
			}
		} else if err != nil {
			t.Fatal(err)
		} else if err := step12NativeClose(file, fileBoundaryUnlockHandle); err != nil {
			t.Fatal(err)
		}
	case "parent-blocked", "parent-available":
		err := os.Rename(path, path+".parent-probe")
		if action == "parent-blocked" {
			if err == nil {
				_ = os.Rename(path+".parent-probe", path)
				t.Fatal("held-lock parent unexpectedly renamed")
			}
			if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
				t.Fatal("unexpected held-parent rename error:", err)
			}
		} else {
			if err != nil {
				t.Fatal("released parent rename:", err)
			}
			if err := os.Rename(path+".parent-probe", path); err != nil {
				t.Fatal(err)
			}
		}
	case "leaf-blocked", "leaf-available":
		before, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		renamed := path + ".probe"
		err = os.Rename(path, renamed)
		if action == "leaf-blocked" {
			if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
				if err == nil {
					_ = os.Rename(renamed, path)
				}
				t.Fatalf("held leaf rename=%v, want sharing violation", err)
			}
			if err := os.Remove(path); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
				t.Fatalf("held leaf delete=%v, want sharing violation", err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("blocked leaf operation changed identity", err)
			}
		} else {
			if err != nil {
				t.Fatal("released leaf rename:", err)
			}
			if err := os.Rename(renamed, path); err != nil {
				t.Fatal(err)
			}
			// Retain a backup name only to restore the fixture's exact inode after
			// deleting its canonical leaf; this is not hard-link isolation evidence.
			if err := os.Link(path, renamed); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal("released canonical leaf delete:", err)
			}
			if err := os.Rename(renamed, path); err != nil {
				t.Fatal("restore released fixture:", err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("restored fixture identity changed", err)
			}
		}
	default:
		t.Fatal("unknown child action:", action)
	}
}

func step12NativeProbe(t *testing.T, path, action string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFileBoundaryNativeLockFollowupSubprocess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SEASPRAK_STEP12_NATIVE_LOCK_HELPER=1", "SEASPRAK_STEP12_NATIVE_LOCK_PATH="+path, "SEASPRAK_STEP12_NATIVE_LOCK_ACTION="+action)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cross-process %s: %v: %s", action, err, out)
	}
	t.Logf("cross-process %s passed", action)
}
