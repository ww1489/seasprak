//go:build windows

package storage

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// New test-only Step 13 directory method gate. This is not a replacement for
// production SyncDir or MOVEFILE_WRITE_THROUGH and performs no publication.
var step13NativeDirectoryInvalid = errors.New("step13 native directory: invalid type or identity")

func step13NativeDirectoryErrno(err error) error {
	var status windows.NTStatus
	if errors.As(err, &status) {
		return status.Errno()
	}
	return err
}

func step13NativeDirectoryOrdinary(info os.FileInfo) bool {
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && info.IsDir() && attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0
}

// Empty native ObjectName references the borrowed Root.Open(".") directory
// itself, not Root.Name(), an absolute reopen, or the replaced directory name.
func step13NativeDirectoryOpen(directory *os.File) (*os.File, error) {
	name, err := windows.NewNTUnicodeString("")
	if err != nil {
		return nil, err
	}
	attrs := &windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(directory.Fd()), ObjectName: name, Attributes: windows.OBJ_DONT_REPARSE | windows.OBJ_CASE_INSENSITIVE}
	attrs.Length = uint32(unsafe.Sizeof(*attrs))
	var h windows.Handle
	err = windows.NtCreateFile(&h, windows.FILE_GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES, attrs, &windows.IO_STATUS_BLOCK{}, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		return nil, step13NativeDirectoryErrno(err)
	}
	file := os.NewFile(uintptr(h), "step13-bound-directory-flush")
	if file == nil {
		return nil, errors.Join(errors.New("wrap native directory"), windows.CloseHandle(h))
	}
	return file, nil
}

// The caller owns both borrowed handles throughout this operation. Only the
// newly opened native handle is closed here, on success and on every failure.
// The sole injection is this call's narrow native opener, never a global hook.
func step13NativeDirectorySync(root *os.Root, directory *os.File, opener func(*os.File) (*os.File, error)) error {
	before, err := root.Lstat(".")
	if err != nil {
		return err
	}
	borrowed, err := directory.Stat()
	if err != nil {
		return err
	}
	if !step13NativeDirectoryOrdinary(before) || !step13NativeDirectoryOrdinary(borrowed) || !os.SameFile(before, borrowed) {
		return step13NativeDirectoryInvalid
	}
	if opener == nil {
		opener = step13NativeDirectoryOpen
	}
	file, err := opener(directory)
	if err != nil {
		return err
	}
	finish := func(err error) error { return errors.Join(err, file.Close()) }
	opened, err := file.Stat()
	if err != nil {
		return finish(err)
	}
	if !step13NativeDirectoryOrdinary(opened) || !os.SameFile(before, opened) {
		return finish(step13NativeDirectoryInvalid)
	}
	current, err := root.Lstat(".")
	if err != nil {
		return finish(err)
	}
	if !step13NativeDirectoryOrdinary(current) || !os.SameFile(opened, current) {
		return finish(step13NativeDirectoryInvalid)
	}
	flushErr := windows.FlushFileBuffers(windows.Handle(file.Fd()))
	if errors.Is(flushErr, windows.ERROR_INVALID_FUNCTION) {
		flushErr = nil // Only this actual Flush error is the existing exception.
	}
	return finish(flushErr)
}

func step13NativeDirectoryFixture(t *testing.T) (*os.Root, *os.File, string) {
	t.Helper()
	state := t.TempDir()
	path := filepath.Join(state, "bound")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	trusted, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = trusted.Close() })
	root, err := fileBoundaryOpenCheckedRoot(trusted, "bound")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	directory, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	return root, directory, path
}

func step13NativeDirectoryAttrs(t *testing.T, info os.FileInfo) uint32 {
	t.Helper()
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		t.Fatal("no complete Windows attributes")
	}
	return attrs.FileAttributes
}

// Augments the existing byte/mode/relative-entry snapshot with every observed
// FileAttributes bit. This deliberately does not claim all metadata or DACLs.
func step13NativeDirectoryTree(t *testing.T, path string) map[string]uint32 {
	t.Helper()
	attrs := map[string]uint32{}
	err := filepath.WalkDir(path, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(name)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(path, name)
		if err != nil {
			return err
		}
		attrs[rel] = step13NativeDirectoryAttrs(t, info)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return attrs
}

func TestFileBoundaryNativeDirectoryChmodBoundObject(t *testing.T) {
	root, directory, path := step13NativeDirectoryFixture(t)
	original, err := directory.Stat()
	if err != nil {
		t.Fatal(err)
	}
	originalAttrs := step13NativeDirectoryAttrs(t, original)
	if err := root.Chmod(".", 0500); err != nil {
		t.Fatal("set retained directory READONLY:", err)
	}
	t.Cleanup(func() { _ = root.Chmod(".", 0700) })
	readonly, err := directory.Stat()
	if err != nil {
		t.Fatal(err)
	}
	readonlyAttrs := step13NativeDirectoryAttrs(t, readonly)
	if readonlyAttrs != originalAttrs|windows.FILE_ATTRIBUTE_READONLY || !os.SameFile(original, readonly) {
		t.Fatalf("fixture did not set actual READONLY on same object: before=%#x after=%#x", originalAttrs, readonlyAttrs)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(outside, "child"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0700) })
	beforeTree, beforeAttrs := fileBoundaryTree(t, outside), step13NativeDirectoryTree(t, outside)
	if beforeAttrs["."]&windows.FILE_ATTRIBUTE_READONLY == 0 {
		t.Fatal("outside fixture must also have READONLY to detect a wrong chmod target")
	}
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal("rename already-bound directory:", err)
	}
	storageDirectoryLink(t, path, outside)
	t.Cleanup(func() { _ = os.Remove(path) })
	// This must really clear READONLY; a no-op Chmod would not prove binding.
	if err := root.Chmod(".", 0700); err != nil {
		t.Fatal("Chmod on bound object after real junction replacement:", err)
	}
	after, err := directory.Stat()
	if err != nil {
		t.Fatal(err)
	}
	afterAttrs := step13NativeDirectoryAttrs(t, after)
	if afterAttrs != readonlyAttrs&^windows.FILE_ATTRIBUTE_READONLY || !os.SameFile(original, after) {
		t.Fatalf("bound Chmod changed wrong attributes/object: before=%#x after=%#x", readonlyAttrs, afterAttrs)
	}
	held, err := os.Stat(path + ".held")
	if err != nil || !os.SameFile(original, held) || step13NativeDirectoryAttrs(t, held) != afterAttrs {
		t.Fatal("retained renamed object attributes/identity:", err)
	}
	if !reflect.DeepEqual(beforeTree, fileBoundaryTree(t, outside)) || !reflect.DeepEqual(beforeAttrs, step13NativeDirectoryTree(t, outside)) {
		t.Fatal("Chmod changed outside complete attributes/bytes/entries")
	}
	t.Logf("actual Root.Chmod(\".\") cleared READONLY on retained object %#x -> %#x; outside full attributes/bytes/entries unchanged", readonlyAttrs, afterAttrs)
}

func TestFileBoundaryNativeDirectoryOpenFlags(t *testing.T) {
	root, directory, path := step13NativeDirectoryFixture(t)
	file, err := step13NativeDirectoryOpen(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	before, err := directory.Stat()
	if err != nil {
		t.Fatal(err)
	}
	opened, err := file.Stat()
	if err != nil || !step13NativeDirectoryOrdinary(opened) || !os.SameFile(before, opened) {
		t.Fatal("raw empty-name directory type/reparse/identity:", err)
	}
	var granted uint32
	err = windows.NtQueryInformationFile(windows.Handle(file.Fd()), &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&granted)), uint32(unsafe.Sizeof(granted)), 8) // FILE_ACCESS_INFORMATION
	if err != nil || granted != windows.FILE_GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES {
		t.Fatalf("actual empty-name rights=%#x, want WRITE|READ_ATTRIBUTES=%#x error=%v", granted, windows.FILE_GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES, step13NativeDirectoryErrno(err))
	}
	if err := os.Rename(path, path+".native-held"); err != nil {
		t.Fatal("new native directory handle must retain READ|WRITE|DELETE sharing:", err)
	}
	flushErr := windows.FlushFileBuffers(windows.Handle(file.Fd()))
	if flushErr != nil && !errors.Is(flushErr, windows.ERROR_INVALID_FUNCTION) {
		t.Fatal("actual raw empty-name FlushFileBuffers:", flushErr)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	plain, err := root.OpenFile("ordinary", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	invalid, typeErr := step13NativeDirectoryOpen(plain)
	if invalid != nil {
		_ = invalid.Close()
		t.Fatal("raw DIRECTORY_FILE accepted an ordinary file")
	}
	var typeCode syscall.Errno
	if !errors.As(typeErr, &typeCode) {
		t.Fatal("raw directory type error did not normalize to OS errno:", typeErr)
	}
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Stat(); err != nil {
		t.Fatal("raw directory open/close affected borrowed handle:", err)
	}
	t.Logf("actual empty-name access=%#x; raw FlushFileBuffers=%v; native-held directory rename passes; raw regular-file rejection errno=%d", granted, flushErr, typeCode)
}

func TestFileBoundaryNativeDirectoryFlushBoundObject(t *testing.T) {
	root, directory, path := step13NativeDirectoryFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	beforeTree, beforeAttrs := fileBoundaryTree(t, outside), step13NativeDirectoryTree(t, outside)
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	storageDirectoryLink(t, path, outside)
	t.Cleanup(func() { _ = os.Remove(path) })
	var captured *os.File
	calls := 0
	err := step13NativeDirectorySync(root, directory, func(directory *os.File) (*os.File, error) {
		calls++
		var err error
		captured, err = step13NativeDirectoryOpen(directory)
		return captured, err
	})
	if err != nil || calls != 1 {
		t.Fatal("actual empty-name WRITE flush:", calls, err)
	}
	if _, err := captured.Stat(); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		t.Fatal("successful flush did not close its new handle:", err)
	}
	if _, err := directory.Stat(); err != nil {
		t.Fatal("flush closed borrowed directory:", err)
	}
	// Directly compare actual read-only-FD Flush. This ACCESS_DENIED is not
	// evidence that the WRITE helper was denied by a filesystem DACL.
	if err := windows.FlushFileBuffers(windows.Handle(directory.Fd())); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatal("actual readonly directory FD Flush:", err)
	}
	if !reflect.DeepEqual(beforeTree, fileBoundaryTree(t, outside)) || !reflect.DeepEqual(beforeAttrs, step13NativeDirectoryTree(t, outside)) {
		t.Fatal("flush changed outside full attributes/bytes/entries")
	}
	if err := os.Rename(path+".held", path+".after-flush"); err != nil {
		t.Fatal("new sync handle leaked sharing restriction:", err)
	}
	t.Log("real empty-name WRITE directory Sync succeeds and closes its handle; readonly FD Flush returns ERROR_ACCESS_DENIED, not helper ACL certification")
}

func TestFileBoundaryNativeDirectorySharingAndClosedHandle(t *testing.T) {
	for _, kind := range []string{"sharing", "closed-directory", "closed-root-after-open", "closed-new-file", "readonly-flush-propagation"} {
		t.Run(kind, func(t *testing.T) {
			root, directory, path := step13NativeDirectoryFixture(t)
			var blocker windows.Handle
			var captured *os.File
			calls := 0
			err := step13NativeDirectorySync(root, directory, func(dir *os.File) (*os.File, error) {
				calls++
				if kind == "sharing" {
					p, err := windows.UTF16PtrFromString(path)
					if err != nil {
						return nil, err
					}
					blocker, err = windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
					if err != nil {
						return nil, err
					}
				} else if kind == "closed-directory" {
					// Deliberate lifetime violation solely to test the real OS
					// error after precheck, not allowed during ordinary borrowing.
					if err := dir.Close(); err != nil {
						return nil, err
					}
				}
				var err error
				if kind == "readonly-flush-propagation" {
					// This tests error propagation, not the exact WRITE opener's
					// ACL failure. The independent DACL test below does that.
					captured, err = root.Open(".")
				} else {
					captured, err = step13NativeDirectoryOpen(dir)
				}
				if err == nil && kind == "closed-root-after-open" {
					err = root.Close()
				} else if err == nil && kind == "closed-new-file" {
					err = captured.Close()
				}
				return captured, err
			})
			if blocker != 0 {
				if err := windows.CloseHandle(blocker); err != nil {
					t.Fatal(err)
				}
			}
			want := error(windows.ERROR_SHARING_VIOLATION)
			switch kind {
			case "closed-directory":
				want = windows.STATUS_OBJECT_TYPE_MISMATCH.Errno()
			case "closed-root-after-open":
				want = os.ErrClosed
			case "closed-new-file":
				want = windows.ERROR_INVALID_HANDLE
			case "readonly-flush-propagation":
				want = windows.ERROR_ACCESS_DENIED
			}
			if calls != 1 || !errors.Is(err, want) {
				t.Fatalf("actual native/flush fault calls=%d err=%v want=%v", calls, err, want)
			}
			if kind == "closed-new-file" && !errors.Is(err, os.ErrClosed) {
				t.Fatal("actual cleanup second-Close error lost:", err)
			}
			if captured != nil {
				if _, err := captured.Stat(); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
					t.Fatal("fault leaked its new directory handle:", err)
				}
			}
			if err := os.Rename(path, path+".released"); err != nil {
				t.Fatal("error cleanup blocked directory rename:", err)
			}
			if kind == "sharing" {
				if err := step13NativeDirectorySync(root, directory, nil); err != nil {
					t.Fatal("actual helper did not recover after blocker Close:", err)
				}
			}
		})
	}
}

func TestFileBoundaryNativeDirectoryRejectsTypeAndIdentity(t *testing.T) {
	for _, kind := range []string{"borrowed-file", "borrowed-other-directory", "opened-file", "opened-other-directory", "opened-junction"} {
		t.Run(kind, func(t *testing.T) {
			root, directory, path := step13NativeDirectoryFixture(t)
			other := t.TempDir()
			if err := os.WriteFile(filepath.Join(other, "ordinary"), []byte("outside sentinel"), 0644); err != nil {
				t.Fatal(err)
			}
			beforeTree, beforeAttrs := fileBoundaryTree(t, other), step13NativeDirectoryTree(t, other)
			var candidate *os.File
			var err error
			if kind == "borrowed-file" || kind == "opened-file" {
				candidate, err = os.Open(filepath.Join(other, "ordinary"))
			} else if kind == "opened-junction" {
				junction := filepath.Join(filepath.Dir(path), "outside-junction")
				storageDirectoryLink(t, junction, other)
				t.Cleanup(func() { _ = os.Remove(junction) })
				p, err := windows.UTF16PtrFromString(junction)
				if err != nil {
					t.Fatal(err)
				}
				h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
				if err != nil {
					t.Fatal(err)
				}
				candidate = os.NewFile(uintptr(h), "step13-junction-type-check")
				if candidate == nil {
					_ = windows.CloseHandle(h)
					t.Fatal("wrap junction fixture")
				}
			} else {
				candidate, err = os.Open(other)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = candidate.Close() })
			calls := 0
			borrowed := directory
			wantCalls := 1
			if kind == "borrowed-file" || kind == "borrowed-other-directory" {
				borrowed = candidate
				wantCalls = 0
			}
			err = step13NativeDirectorySync(root, borrowed, func(*os.File) (*os.File, error) {
				calls++
				return candidate, nil
			})
			if calls != wantCalls || !errors.Is(err, step13NativeDirectoryInvalid) {
				t.Fatalf("type/identity reject calls=%d err=%v", calls, err)
			}
			if wantCalls == 1 {
				if _, err := candidate.Stat(); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
					t.Fatal("failed new-handle validation did not close:", err)
				}
			} else if _, err := candidate.Stat(); err != nil {
				t.Fatal("failed precheck closed a borrowed handle:", err)
			}
			if err := candidate.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				t.Fatal(err)
			}
			if err := os.Rename(other, other+".released"); err != nil {
				t.Fatal("identity/type reject leaked handle:", err)
			}
			if !reflect.DeepEqual(beforeTree, fileBoundaryTree(t, other+".released")) || !reflect.DeepEqual(beforeAttrs, step13NativeDirectoryTree(t, other+".released")) {
				t.Fatal("type/identity check changed outside full attributes/bytes/entries")
			}
		})
	}
}

func TestFileBoundaryNativeDirectoryDACLAccessDenied(t *testing.T) {
	root, directory, path := step13NativeDirectoryFixture(t)
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Hold restoration rights before adding a deny ACE on this one temporary
	// directory. No token privilege is enabled and no machine ACL is changed.
	h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Skipf("temporary DACL restoration handle unavailable; exact WRITE-helper ACL failure unverified: %v", err)
	}
	defer func() {
		if err := windows.CloseHandle(h); err != nil {
			t.Error("close DACL restoration handle:", err)
		}
	}()
	original, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Skipf("temporary DACL snapshot unavailable; helper ACL failure unverified: %v", err)
	}
	originalDACL, _, err := original.DACL()
	if err != nil || originalDACL == nil {
		t.Skip("temporary DACL unavailable; helper ACL failure unverified")
	}
	control, _, err := original.Control()
	if err != nil {
		t.Fatal(err)
	}
	restoreFlags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		restoreFlags = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	restore := func() error {
		err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, restoreFlags, nil, nil, originalDACL, nil)
		runtime.KeepAlive(original)
		return err
	}
	world, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		t.Fatal(err)
	}
	var pin runtime.Pinner
	pin.Pin(world)
	defer pin.Unpin()
	deny, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES,
		AccessMode:        windows.DENY_ACCESS,
		Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(world)},
	}}, originalDACL)
	if err != nil {
		t.Skipf("temporary deny DACL construction unavailable; helper ACL failure unverified: %v", err)
	}
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, deny, nil); err != nil {
		t.Skipf("temporary deny DACL application unavailable; helper ACL failure unverified: %v", err)
	}
	defer func() {
		if err := restore(); err != nil {
			t.Error("restore exact temporary DACL:", err)
		}
	}()
	calls := 0
	var captured *os.File
	err = step13NativeDirectorySync(root, directory, func(dir *os.File) (*os.File, error) {
		calls++
		var err error
		captured, err = step13NativeDirectoryOpen(dir)
		return captured, err
	})
	if captured != nil {
		// An environment with pre-enabled backup/restore privileges might not
		// enforce this fixture. Never claim a synthetic failure is ACL proof.
		if _, statErr := captured.Stat(); !errors.Is(statErr, windows.ERROR_INVALID_HANDLE) {
			_ = captured.Close()
			t.Error("successful native handle was not closed by helper:", statErr)
		}
	}
	if calls != 1 || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		if err == nil && calls == 1 {
			t.Skip("actual configured environment bypasses temporary deny; exact WRITE-helper ACL failure remains unverified")
		}
		t.Fatalf("actual helper ACL failure calls=%d err=%v", calls, err)
	}
	if captured != nil {
		t.Fatal("denied native open returned a file")
	}
	if err := restore(); err != nil {
		t.Fatal("restore temporary DACL:", err)
	}
	restored, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || restored.String() != original.String() {
		t.Fatal("temporary DACL restoration did not match original descriptor:", err)
	}
	if err := step13NativeDirectorySync(root, directory, nil); err != nil {
		t.Fatal("WRITE helper did not recover after restoring DACL:", err)
	}
	t.Log("actual empty-name FILE_GENERIC_WRITE helper returns ERROR_ACCESS_DENIED under temporary deny ACE; exact original DACL restored; helper then succeeds")
}
