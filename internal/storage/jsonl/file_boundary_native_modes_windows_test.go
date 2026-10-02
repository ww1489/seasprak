//go:build windows

package jsonl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/gofrs/flock"
	"golang.org/x/sys/windows"
)

// New test-only Step 13 method gate; Store, Repair and all production files
// remain unchanged. These sentinels are not public product error codes.
var step13NativeInvalid = errors.New("step13 native: invalid type or identity")
var step13NativeBusy = errors.New("step13 native: lock busy")

type step13NativeMode int

const (
	step13NativeLock step13NativeMode = iota
	step13NativeJournalReader
	step13NativeJournalExisting
	step13NativeJournalNew
)

func (m step13NativeMode) leaf() string {
	if m == step13NativeLock {
		return "writer.lock"
	}
	return "journal.jsonl"
}

func (m step13NativeMode) access() uint32 {
	if m == step13NativeJournalReader {
		return windows.FILE_GENERIC_READ
	}
	// flock's O_CREATE|O_RDONLY adds WRITE in Go's Windows syscall.Open,
	// including for an existing leaf. Journal writers use O_RDWR.
	return windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE
}

func (m step13NativeMode) oldFlags() int {
	switch m {
	case step13NativeLock:
		return os.O_CREATE | os.O_RDONLY
	case step13NativeJournalReader:
		return os.O_RDONLY
	case step13NativeJournalExisting:
		return os.O_RDWR
	default:
		return os.O_CREATE | os.O_RDWR
	}
}

func step13NativeErrno(err error) error {
	var status windows.NTStatus
	if errors.As(err, &status) {
		return status.Errno()
	}
	return err
}

// This raw native opener has only the two fixed leaves. The case flag may be
// omitted solely for the explicit negative control below, never as a fallback.
func step13NativeRawOpen(directory *os.File, mode step13NativeMode, create, caseInsensitive bool) (*os.File, error) {
	name, err := windows.NewNTUnicodeString(mode.leaf())
	if err != nil {
		return nil, err
	}
	flags := uint32(windows.OBJ_DONT_REPARSE)
	if caseInsensitive {
		flags |= windows.OBJ_CASE_INSENSITIVE
	}
	attrs := &windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(directory.Fd()), ObjectName: name, Attributes: flags}
	attrs.Length = uint32(unsafe.Sizeof(*attrs))
	disposition := uint32(windows.FILE_OPEN)
	if create {
		disposition = windows.FILE_CREATE // Never OPEN_IF, overwrite or truncate.
	}
	var h windows.Handle
	err = windows.NtCreateFile(&h, mode.access(), attrs, &windows.IO_STATUS_BLOCK{}, nil, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, disposition,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		return nil, step13NativeErrno(err)
	}
	file := os.NewFile(uintptr(h), "step13-relative-"+mode.leaf())
	if file == nil {
		return nil, errors.Join(errors.New("wrap native leaf handle"), windows.CloseHandle(h))
	}
	return file, nil
}

func step13NativeOpen(directory *os.File, mode step13NativeMode, create bool) (*os.File, error) {
	return step13NativeRawOpen(directory, mode, create, true)
}

func step13NativeOrdinary(info os.FileInfo, directory bool) bool {
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok || attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false
	}
	if directory {
		return info.IsDir()
	}
	return info.Mode().IsRegular()
}

// Root and directory are borrowed for the entire call and must not be closed
// concurrently. A successful result owns only its new file handle. Injection
// is per call at precisely the real native opener, not a global/abstract FS.
func step13NativeCheckedOpen(root *os.Root, directory *os.File, mode step13NativeMode, opener func(*os.File, step13NativeMode, bool) (*os.File, error)) (*os.File, error) {
	bound, err := root.Lstat(".")
	if err != nil {
		return nil, err
	}
	borrowed, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	if !step13NativeOrdinary(bound, true) || !step13NativeOrdinary(borrowed, true) || !os.SameFile(bound, borrowed) {
		return nil, step13NativeInvalid
	}
	before, err := root.Lstat(mode.leaf())
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return nil, err
	}
	create := mode == step13NativeJournalNew || (mode == step13NativeLock && missing)
	if missing && !create {
		return nil, err
	}
	if !missing && !step13NativeOrdinary(before, false) {
		return nil, step13NativeInvalid
	}
	if opener == nil {
		opener = step13NativeOpen
	}
	file, err := opener(directory, mode, create)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) { return nil, errors.Join(err, file.Close()) }
	opened, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	if !step13NativeOrdinary(opened, false) || (!create && !os.SameFile(before, opened)) {
		return fail(step13NativeInvalid)
	}
	current, err := root.Lstat(mode.leaf())
	if err != nil {
		return fail(err)
	}
	if !step13NativeOrdinary(current, false) || !os.SameFile(opened, current) {
		return fail(step13NativeInvalid)
	}
	return file, nil
}

func step13NativeAcquire(root *os.Root, directory *os.File) (*os.File, error) {
	file, err := step13NativeCheckedOpen(root, directory, step13NativeLock, nil)
	if err != nil {
		return nil, err
	}
	ok, err := fileBoundaryTryHandleLock(file)
	if err != nil || !ok {
		if err == nil {
			err = step13NativeBusy
		}
		return nil, errors.Join(err, file.Close())
	}
	current, err := root.Lstat("writer.lock")
	if err != nil || !step13NativeOrdinary(current, false) {
		return nil, errors.Join(step13NativeInvalid, err, step13NativeCloseLock(file, fileBoundaryUnlockHandle))
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(opened, current) {
		return nil, errors.Join(step13NativeInvalid, err, step13NativeCloseLock(file, fileBoundaryUnlockHandle))
	}
	return file, nil
}

func step13NativeCloseLock(file *os.File, unlock func(*os.File) error) error {
	unlockErr := unlock(file)
	closeErr := file.Close() // Still attempted if UnlockFileEx fails.
	return errors.Join(unlockErr, closeErr)
}

func step13NativeAccess(t *testing.T, file *os.File) uint32 {
	t.Helper()
	// FILE_ACCESS_INFORMATION (class 8) is a single ACCESS_MASK. Query the
	// actual granted rights, not just the flags we requested in our own code.
	var granted uint32
	err := windows.NtQueryInformationFile(windows.Handle(file.Fd()), &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&granted)), uint32(unsafe.Sizeof(granted)), 8)
	if err != nil {
		t.Fatal("query actual granted rights:", step13NativeErrno(err))
	}
	return granted
}

func step13NativeBytes(t *testing.T, file *os.File) []byte {
	t.Helper()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func step13NativeExerciseIO(t *testing.T, file *os.File, readOnly bool, initial []byte) {
	t.Helper()
	if got := step13NativeBytes(t, file); !bytes.Equal(got, initial) {
		t.Fatalf("initial bytes=%q, want %q (existing opens must not truncate)", got, initial)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	n, err := file.Write([]byte("W"))
	if readOnly {
		if n != 0 || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Fatalf("readonly Write n=%d error=%v", n, err)
		}
		if _, err := file.Seek(0, io.SeekEnd); err != nil {
			t.Fatal(err)
		}
		if n, err := file.Write([]byte("append")); n != 0 || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Fatalf("readonly append n=%d error=%v", n, err)
		}
		if got := step13NativeBytes(t, file); !bytes.Equal(got, initial) {
			t.Fatalf("readonly failed writes changed bytes: %q", got)
		}
		// No readonly Sync: it is an active flush even when it fails.
		return
	}
	if n != 1 || err != nil {
		t.Fatalf("Write n=%d error=%v", n, err)
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if n, err := file.Write([]byte("+append")); n != 7 || err != nil {
		t.Fatalf("append n=%d error=%v", n, err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal("actual writer Sync:", err)
	}
	want := []byte("W+append")
	if len(initial) > 0 {
		want = append(append([]byte("W"), initial[1:]...), []byte("+append")...)
	}
	if got := step13NativeBytes(t, file); !bytes.Equal(got, want) {
		t.Fatalf("written bytes=%q want %q", got, want)
	}
}

func TestFileBoundaryNativeModesLockAccessBaseline(t *testing.T) {
	for _, kind := range []string{"new", "existing"} {
		t.Run(kind, func(t *testing.T) {
			for _, method := range []string{"old-open", "native"} {
				t.Run(method, func(t *testing.T) {
					root, directory, path := step12NativeFixture(t)
					initial := []byte{}
					if kind == "existing" {
						initial = []byte("existing lock sentinel")
						if err := os.WriteFile(filepath.Join(path, "writer.lock"), initial, 0600); err != nil {
							t.Fatal(err)
						}
					}
					var file *os.File
					var err error
					if method == "old-open" {
						file, err = os.OpenFile(filepath.Join(path, "writer.lock"), os.O_CREATE|os.O_RDONLY, 0600)
					} else {
						file, err = step13NativeCheckedOpen(root, directory, step13NativeLock, nil)
					}
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = file.Close() })
					granted := step13NativeAccess(t, file)
					if granted != step13NativeLock.access() {
						t.Fatalf("actual %s rights=%#x, want original READ|WRITE=%#x", method, granted, step13NativeLock.access())
					}
					step13NativeExerciseIO(t, file, false, initial)
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
					t.Logf("%s %s actual access=%#x; Read/Write/SeekEnd+Write/Sync pass", method, kind, granted)
				})
			}
		})
	}
}

func TestFileBoundaryNativeModesCaseInsensitive(t *testing.T) {
	for _, mode := range []step13NativeMode{step13NativeLock, step13NativeJournalReader, step13NativeJournalExisting, step13NativeJournalNew} {
		t.Run(mode.leaf()+"/"+step13NativeModeName(mode), func(t *testing.T) {
			root, directory, path := step12NativeFixture(t)
			upper := strings.ToUpper(mode.leaf())
			payload := []byte("case sentinel")
			if err := os.WriteFile(filepath.Join(path, upper), payload, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := root.Lstat(upper)
			if err != nil {
				t.Fatal(err)
			}
			// A true native control, not a fake error. Some filesystem policy may
			// remain insensitive without OBJ_CASE_INSENSITIVE; record that fact.
			control, controlErr := step13NativeRawOpen(directory, mode, false, false)
			if control != nil {
				info, err := control.Stat()
				if err != nil || !os.SameFile(before, info) {
					_ = control.Close()
					t.Fatal("case control returned another object:", err)
				}
				if err := control.Close(); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(controlErr, windows.ERROR_FILE_NOT_FOUND) {
				t.Fatal("unexpected case-sensitive control error:", controlErr)
			}
			file, err := step13NativeCheckedOpen(root, directory, mode, nil)
			if mode == step13NativeJournalNew {
				if file != nil || !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
					if file != nil {
						_ = file.Close()
					}
					t.Fatalf("case-insensitive FILE_CREATE collision=%v", err)
				}
			} else {
				if err != nil {
					t.Fatal("OBJ_CASE_INSENSITIVE fixed lowercase name:", err)
				}
				info, err := file.Stat()
				if err != nil || !os.SameFile(before, info) || !bytes.Equal(step13NativeBytes(t, file), payload) {
					_ = file.Close()
					t.Fatal("uppercase leaf did not retain identity/bytes:", err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			current, err := root.Lstat(upper)
			if err != nil || !os.SameFile(before, current) {
				t.Fatal("case-insensitive open/collision changed leaf identity:", err)
			}
			entries, err := os.ReadDir(path)
			if err != nil || len(entries) != 1 || entries[0].Name() != upper {
				t.Fatal("case lookup created or changed a leaf:", entries, err)
			}
			got, err := os.ReadFile(filepath.Join(path, upper))
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatal("case collision changed original bytes:", err)
			}
			t.Logf("OBJ_CASE_INSENSITIVE: actual SameFile/collision pass; omitted-flag control error=%v", controlErr)
		})
	}
}

func step13NativeModeName(mode step13NativeMode) string {
	switch mode {
	case step13NativeLock:
		return "lock"
	case step13NativeJournalReader:
		return "reader"
	case step13NativeJournalExisting:
		return "writer-existing"
	default:
		return "writer-new"
	}
}

func TestFileBoundaryNativeModesLockInterop(t *testing.T) {
	root, directory, path := step12NativeFixture(t)
	file, err := step13NativeAcquire(root, directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	lockPath := filepath.Join(path, "writer.lock")
	step12NativeProbe(t, lockPath, "legacy-busy")
	step12NativeProbe(t, lockPath, "leaf-blocked")
	if err := fileBoundaryUnlockHandle(file); err != nil {
		t.Fatal(err)
	}
	// Unlike Step 12, probe after Unlock but before closing the no-DELETE FD.
	step12NativeProbe(t, lockPath, "legacy-available")
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	step12NativeProbe(t, lockPath, "leaf-available")
	legacy := flock.New(lockPath)
	ok, err := legacy.TryLock()
	if err != nil || !ok {
		t.Fatal("actual old flock:", ok, err)
	}
	t.Cleanup(func() { _ = legacy.Unlock() })
	step13NativeProbe(t, lockPath, "native-busy")
	if err := legacy.Unlock(); err != nil {
		t.Fatal(err)
	}
	step13NativeProbe(t, lockPath, "native-available")
	file, err = step13NativeAcquire(root, directory)
	if err != nil {
		t.Fatal("READ|WRITE close-alone acquisition:", err)
	}
	if err := file.Close(); err != nil { // No explicit Unlock in this branch.
		t.Fatal(err)
	}
	ok, err = legacy.TryLock()
	if err != nil || !ok {
		t.Fatal("actual old flock after native Close alone:", ok, err)
	}
	if err := legacy.Unlock(); err != nil {
		t.Fatal(err)
	}
	t.Log("READ|WRITE candidate retains bidirectional old-flock exclusion; Unlock alone and Close alone release byte-range lock; Close releases deny-DELETE")
}

func TestFileBoundaryNativeModesJournalIOAndSharing(t *testing.T) {
	for _, mode := range []step13NativeMode{step13NativeJournalReader, step13NativeJournalExisting, step13NativeJournalNew} {
		t.Run(step13NativeModeName(mode), func(t *testing.T) {
			for _, method := range []string{"old-open", "native"} {
				t.Run(method, func(t *testing.T) {
					root, directory, path := step12NativeFixture(t)
					journalPath := filepath.Join(path, "journal.jsonl")
					initial := []byte{}
					if mode != step13NativeJournalNew {
						initial = []byte("journal sentinel\n")
						if err := os.WriteFile(journalPath, initial, 0600); err != nil {
							t.Fatal(err)
						}
					}
					var file *os.File
					var err error
					if method == "old-open" {
						file, err = os.OpenFile(journalPath, mode.oldFlags(), 0600)
					} else {
						file, err = step13NativeCheckedOpen(root, directory, mode, nil)
					}
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = file.Close() })
					granted := step13NativeAccess(t, file)
					if granted != mode.access() {
						t.Fatalf("actual granted access=%#x, want %#x", granted, mode.access())
					}
					step13NativeExerciseIO(t, file, mode == step13NativeJournalReader, initial)
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
					t.Logf("%s access=%#x; readonly Sync calls=0; writer actual Sync tested", method, granted)
				})
			}
			root, directory, path := step12NativeFixture(t)
			journalPath := filepath.Join(path, "journal.jsonl")
			if mode != step13NativeJournalNew {
				if err := os.WriteFile(journalPath, []byte("coexistence sentinel"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			first, err := step13NativeCheckedOpen(root, directory, mode, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = first.Close() })
			otherMode := step13NativeJournalReader
			if mode == step13NativeJournalReader {
				otherMode = step13NativeJournalExisting
			}
			second, err := step13NativeCheckedOpen(root, directory, otherMode, nil)
			if err != nil {
				t.Fatal("reader and writer must coexist:", err)
			}
			t.Cleanup(func() { _ = second.Close() })
			firstInfo, err := first.Stat()
			if err != nil {
				t.Fatal(err)
			}
			secondInfo, err := second.Stat()
			if err != nil || !os.SameFile(firstInfo, secondInfo) {
				t.Fatal("coexisting reader/writer identity:", err)
			}
			step12NativeProbe(t, journalPath, "leaf-blocked")
			if err := second.Close(); err != nil {
				t.Fatal(err)
			}
			step12NativeProbe(t, journalPath, "leaf-blocked")
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			step12NativeProbe(t, journalPath, "leaf-available")
		})
	}
}

func TestFileBoundaryNativeModesJournalParentPolicy(t *testing.T) {
	for _, mode := range []step13NativeMode{step13NativeJournalReader, step13NativeJournalExisting, step13NativeJournalNew} {
		t.Run(step13NativeModeName(mode), func(t *testing.T) {
			var oldErrno syscall.Errno
			for _, method := range []string{"old-open", "native"} {
				t.Run(method, func(t *testing.T) {
					root, directory, path := step12NativeFixture(t)
					if mode != step13NativeJournalNew {
						if err := os.WriteFile(filepath.Join(path, "journal.jsonl"), []byte("parent policy"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					var file *os.File
					var err error
					if method == "old-open" {
						file, err = os.OpenFile(filepath.Join(path, "journal.jsonl"), mode.oldFlags(), 0600)
					} else {
						file, err = step13NativeCheckedOpen(root, directory, mode, nil)
					}
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = file.Close() })
					err = os.Rename(path, path+".parent")
					var code syscall.Errno
					if !errors.As(err, &code) || (code != windows.ERROR_ACCESS_DENIED && code != windows.ERROR_SHARING_VIOLATION) {
						if err == nil {
							_ = os.Rename(path+".parent", path)
						}
						t.Fatalf("held parent rename=%v", err)
					}
					if method == "old-open" {
						oldErrno = code
					} else if code != oldErrno {
						t.Fatalf("native parent errno=%d, original=%d", code, oldErrno)
					}
					step12NativeProbe(t, path, "parent-blocked")
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
					step12NativeProbe(t, path, "parent-available")
					t.Logf("%s parent rename errno=%d; Close restores rename with Root/directory still open", method, code)
				})
			}
		})
	}
}

// Snapshot full Windows FileAttributes in addition to the existing bytes/mode
// and relative-entry snapshot. No claim is made about ACLs or all metadata.
func step13NativeTree(t *testing.T, path string) map[string]uint32 {
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
		sys, ok := info.Sys().(*syscall.Win32FileAttributeData)
		if !ok {
			return step13NativeInvalid
		}
		rel, err := filepath.Rel(path, name)
		if err != nil {
			return err
		}
		attrs[rel] = sys.FileAttributes
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return attrs
}

func TestFileBoundaryNativeModesJournalBoundDirectory(t *testing.T) {
	for _, mode := range []step13NativeMode{step13NativeJournalReader, step13NativeJournalExisting, step13NativeJournalNew} {
		t.Run(step13NativeModeName(mode), func(t *testing.T) {
			root, directory, path := step12NativeFixture(t)
			initial := []byte{}
			if mode != step13NativeJournalNew {
				initial = []byte("trusted journal")
				if err := os.WriteFile(filepath.Join(path, "journal.jsonl"), initial, 0600); err != nil {
					t.Fatal(err)
				}
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "journal.jsonl"), []byte("outside journal"), 0644); err != nil {
				t.Fatal(err)
			}
			beforeTree, beforeAttrs := fileBoundaryTree(t, outside), step13NativeTree(t, outside)
			if err := os.Rename(path, path+".held"); err != nil {
				t.Fatal(err)
			}
			fileBoundaryDirectoryLink(t, path, outside)
			file, err := step13NativeCheckedOpen(root, directory, mode, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			step13NativeExerciseIO(t, file, mode == step13NativeJournalReader, initial)
			opened, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			held, err := os.Stat(filepath.Join(path+".held", "journal.jsonl"))
			if err != nil || !os.SameFile(opened, held) {
				t.Fatal("journal did not land in retained bound directory:", err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeTree, fileBoundaryTree(t, outside)) || !reflect.DeepEqual(beforeAttrs, step13NativeTree(t, outside)) {
				t.Fatal("outside bytes/modes/entries/full attributes changed")
			}
			t.Log("real junction replacement: native journal stays bound; outside full attributes/bytes/entries unchanged")
		})
	}
}

// Channel barriers bracket only this call's real native opener. A timeout is
// a hang guard, not a sleep, polling loop or filesystem-race probability.
func step13NativeInterleave(t *testing.T, root *os.Root, directory *os.File, mode step13NativeMode, change func()) (*os.File, *os.File, error, int) {
	t.Helper()
	reached, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	type result struct {
		file, opened *os.File
		err          error
		calls        int
	}
	done := make(chan result, 1)
	finished := false
	go func() {
		var opened *os.File
		calls := 0
		file, err := step13NativeCheckedOpen(root, directory, mode, func(dir *os.File, mode step13NativeMode, create bool) (*os.File, error) {
			calls++
			close(reached)
			<-resume
			var err error
			opened, err = step13NativeOpen(dir, mode, create)
			return opened, err
		})
		done <- result{file, opened, err, calls}
	}()
	t.Cleanup(func() {
		unblock()
		if !finished {
			select {
			case got := <-done:
				if got.file != nil {
					_ = got.file.Close()
				}
			case <-time.After(10 * time.Second):
				t.Error("native opener did not finish")
			}
		}
	})
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("native opener did not reach boundary")
	}
	change()
	unblock()
	select {
	case got := <-done:
		finished = true
		return got.file, got.opened, got.err, got.calls
	case <-time.After(10 * time.Second):
		t.Fatal("native opener did not return")
		return nil, nil, nil, 0
	}
}

func TestFileBoundaryNativeModesRejectsLeavesAndIdentity(t *testing.T) {
	for _, mode := range []step13NativeMode{step13NativeLock, step13NativeJournalReader, step13NativeJournalExisting} {
		for _, kind := range []string{"directory", "junction", "file-symlink", "ordinary-A-to-B", "dynamic-file-symlink"} {
			t.Run(step13NativeModeName(mode)+"/"+kind, func(t *testing.T) {
				root, directory, path := step12NativeFixture(t)
				outside := t.TempDir()
				external := filepath.Join(outside, "ordinary-B")
				if err := os.WriteFile(external, []byte("external sentinel"), 0644); err != nil {
					t.Fatal(err)
				}
				leaf := filepath.Join(path, mode.leaf())
				switch kind {
				case "directory":
					if err := os.Mkdir(leaf, 0700); err != nil {
						t.Fatal(err)
					}
				case "junction":
					fileBoundaryDirectoryLink(t, leaf, outside)
				case "file-symlink":
					fileBoundaryFileLink(t, leaf, external)
				default:
					if err := os.WriteFile(leaf, []byte("ordinary A"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				beforeTree, beforeAttrs := fileBoundaryTree(t, outside), step13NativeTree(t, outside)
				calls := 0
				var file, opened *os.File
				var err error
				if kind == "ordinary-A-to-B" || kind == "dynamic-file-symlink" {
					file, opened, err, calls = step13NativeInterleave(t, root, directory, mode, func() {
						if err := os.Rename(leaf, leaf+".held"); err != nil {
							t.Fatal(err)
						}
						if kind == "dynamic-file-symlink" {
							fileBoundaryFileLink(t, leaf, external)
						} else {
							// A hard link supplies a known B inode, not a claim of
							// general hard-link isolation from hostile local users.
							if err := os.Link(external, leaf); err != nil {
								t.Fatal(err)
							}
						}
					})
				} else {
					file, err = step13NativeCheckedOpen(root, directory, mode, func(dir *os.File, mode step13NativeMode, create bool) (*os.File, error) {
						calls++
						return step13NativeOpen(dir, mode, create)
					})
				}
				if file != nil || !errors.Is(err, step13NativeInvalid) {
					if file != nil {
						_ = file.Close()
					}
					t.Fatalf("invalid leaf file=%v err=%v", file, err)
				}
				wantCalls := 0
				if kind == "ordinary-A-to-B" || kind == "dynamic-file-symlink" {
					wantCalls = 1
					if opened == nil {
						t.Fatal("identity check never opened the replaced B object")
					}
					if _, err := opened.Stat(); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
						t.Fatal("rejection failed to close new no-DELETE handle:", err)
					}
					if err := os.Rename(leaf, leaf+".rejected"); err != nil {
						t.Fatal("rejected handle leaked deny-DELETE:", err)
					}
					competitor := flock.New(external)
					ok, err := competitor.TryLock()
					if err != nil || !ok {
						t.Fatal("competition after reject:", ok, err)
					}
					if err := competitor.Unlock(); err != nil {
						t.Fatal(err)
					}
				} else {
					raw, rawErr := step13NativeOpen(directory, mode, false)
					if raw != nil {
						// OPEN_REPARSE_POINT can open the reparse leaf itself.
						// The checked opener must reject its full attributes;
						// it must not claim that native flags alone reject it.
						info, statErr := raw.Stat()
						linkInfo, linkErr := root.Lstat(mode.leaf())
						externalInfo, extErr := os.Stat(external)
						closeErr := raw.Close()
						if statErr != nil || linkErr != nil || extErr != nil || closeErr != nil || step13NativeOrdinary(info, false) || !os.SameFile(info, linkInfo) || os.SameFile(info, externalInfo) {
							t.Fatal("raw no-follow reparse leaf identity/type/close:", statErr, linkErr, extErr, closeErr)
						}
						if err := os.Rename(leaf, leaf+".rawclosed"); err != nil {
							t.Fatal("raw reparse handle did not close:", err)
						}
						if err := os.Rename(leaf+".rawclosed", leaf); err != nil {
							t.Fatal(err)
						}
						t.Log("raw native opens reparse leaf itself; full-attribute checked opener rejects before native call")
					} else {
						var code syscall.Errno
						if !errors.As(rawErr, &code) {
							t.Fatalf("native error was not normalized OS errno: %v", rawErr)
						}
						t.Logf("real raw invalid leaf errno=%d", code)
					}
				}
				if calls != wantCalls {
					t.Fatalf("native calls=%d want %d", calls, wantCalls)
				}
				if !reflect.DeepEqual(beforeTree, fileBoundaryTree(t, outside)) || !reflect.DeepEqual(beforeAttrs, step13NativeTree(t, outside)) {
					t.Fatal("rejected leaf changed outside bytes/full attributes/entries")
				}
			})
		}
	}
}

func TestFileBoundaryNativeModesCreateCollision(t *testing.T) {
	for _, mode := range []step13NativeMode{step13NativeLock, step13NativeJournalNew} {
		t.Run(step13NativeModeName(mode), func(t *testing.T) {
			root, directory, path := step12NativeFixture(t)
			leaf := filepath.Join(path, mode.leaf())
			file, _, err, calls := step13NativeInterleave(t, root, directory, mode, func() {
				if err := os.WriteFile(leaf, []byte("collision sentinel"), 0600); err != nil {
					t.Fatal(err)
				}
			})
			if file != nil || calls != 1 || !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
				if file != nil {
					_ = file.Close()
				}
				t.Fatalf("FILE_CREATE collision calls=%d err=%v", calls, err)
			}
			data, err := os.ReadFile(leaf)
			if err != nil || string(data) != "collision sentinel" {
				t.Fatal("FILE_CREATE collision truncated/changed leaf:", err)
			}
			if err := os.Rename(leaf, leaf+".released"); err != nil {
				t.Fatal("collision leaked handle:", err)
			}
		})
	}
}

func TestFileBoundaryNativeModesNativeErrorsAndClose(t *testing.T) {
	for _, mode := range []step13NativeMode{step13NativeLock, step13NativeJournalReader, step13NativeJournalExisting, step13NativeJournalNew} {
		for _, kind := range []string{"sharing", "closed-directory", "closed-root-after-open", "closed-file-after-open"} {
			t.Run(step13NativeModeName(mode)+"/"+kind, func(t *testing.T) {
				root, directory, path := step12NativeFixture(t)
				leaf := filepath.Join(path, mode.leaf())
				if mode != step13NativeJournalNew {
					if err := os.WriteFile(leaf, []byte("error sentinel"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				calls := 0
				var captured *os.File
				var blocker windows.Handle
				file, err := step13NativeCheckedOpen(root, directory, mode, func(dir *os.File, mode step13NativeMode, create bool) (*os.File, error) {
					calls++
					if kind == "sharing" {
						p, err := windows.UTF16PtrFromString(leaf)
						if err != nil {
							return nil, err
						}
						blocker, err = windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
						if err != nil {
							return nil, err
						}
					} else if kind == "closed-directory" {
						// Deliberate invalid-lifetime negative test, not an allowed
						// caller action during an ordinary borrowed-handle call.
						if err := dir.Close(); err != nil {
							return nil, err
						}
					}
					var err error
					captured, err = step13NativeOpen(dir, mode, create)
					if err == nil && kind == "closed-root-after-open" {
						err = root.Close()
					} else if err == nil && kind == "closed-file-after-open" {
						err = captured.Close()
					}
					return captured, err
				})
				if blocker != 0 {
					if err := windows.CloseHandle(blocker); err != nil {
						t.Fatal(err)
					}
				}
				if file != nil {
					_ = file.Close()
					t.Fatal("fault returned usable handle")
				}
				want := error(windows.ERROR_SHARING_VIOLATION)
				if mode == step13NativeJournalNew && kind == "sharing" {
					// Exclusive-create reports the blocker-created name collision
					// before sharing checks; it never falls back to FILE_OPEN.
					want = windows.ERROR_ALREADY_EXISTS
				} else if kind == "closed-directory" {
					want = windows.STATUS_OBJECT_TYPE_MISMATCH.Errno()
				} else if kind == "closed-root-after-open" {
					want = os.ErrClosed
				} else if kind == "closed-file-after-open" {
					want = windows.ERROR_INVALID_HANDLE
				}
				if calls != 1 || !errors.Is(err, want) {
					t.Fatalf("real fault calls=%d err=%v want=%v", calls, err, want)
				}
				if kind == "closed-file-after-open" && !errors.Is(err, os.ErrClosed) {
					t.Fatal("cleanup's actual second Close error was lost:", err)
				}
				if captured != nil {
					if _, err := captured.Stat(); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
						t.Fatal("failed validation leaked opened handle:", err)
					}
				}
				if mode == step13NativeJournalNew && kind == "closed-directory" {
					if _, err := os.Lstat(leaf); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("closed directory unexpectedly created a journal:", err)
					}
					if err := os.WriteFile(leaf, []byte("created after failure"), 0600); err != nil {
						t.Fatal("creation after native failure:", err)
					}
				}
				if err := os.Rename(leaf, leaf+".released"); err != nil {
					t.Fatal("failed native call leaked deny-DELETE:", err)
				}
			})
		}
	}
	root, directory, path := step12NativeFixture(t)
	file, err := step13NativeAcquire(root, directory)
	if err != nil {
		t.Fatal(err)
	}
	err = step13NativeCloseLock(file, func(file *os.File) error {
		return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{Offset: 1})
	})
	if !errors.Is(err, windows.ERROR_NOT_LOCKED) {
		t.Fatal("actual wrong-range UnlockFileEx error lost:", err)
	}
	competitor := flock.New(filepath.Join(path, "writer.lock"))
	ok, lockErr := competitor.TryLock()
	if lockErr != nil || !ok {
		t.Fatal("old flock after failed Unlock and remaining Close:", ok, lockErr)
	}
	if err := competitor.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(path, "writer.lock"), filepath.Join(path, "released.lock")); err != nil {
		t.Fatal("unlock failure prevented remaining Close:", err)
	}
	err = step13NativeCloseLock(file, fileBoundaryUnlockHandle)
	if !errors.Is(err, windows.ERROR_INVALID_HANDLE) || !errors.Is(err, os.ErrClosed) {
		t.Fatal("second cleanup did not retain both real errors:", err)
	}
}

func TestFileBoundaryNativeModesReadonlyAttribute(t *testing.T) {
	for _, mode := range []step13NativeMode{step13NativeLock, step13NativeJournalReader, step13NativeJournalExisting} {
		t.Run(step13NativeModeName(mode), func(t *testing.T) {
			root, directory, path := step12NativeFixture(t)
			leaf := filepath.Join(path, mode.leaf())
			if err := os.WriteFile(leaf, []byte("readonly attribute"), 0400); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(leaf, 0600) })
			old, oldErr := os.OpenFile(leaf, mode.oldFlags(), 0600)
			native, nativeErr := step13NativeCheckedOpen(root, directory, mode, nil)
			if mode == step13NativeJournalReader {
				if oldErr != nil || nativeErr != nil {
					t.Fatal("readonly journal should retain read permission:", oldErr, nativeErr)
				}
				step13NativeExerciseIO(t, native, true, []byte("readonly attribute"))
			} else if !errors.Is(oldErr, windows.ERROR_ACCESS_DENIED) || !errors.Is(nativeErr, windows.ERROR_ACCESS_DENIED) {
				t.Errorf("WRITE rights must enforce same READONLY constraint: old=%v native=%v", oldErr, nativeErr)
			}
			if old != nil {
				if err := old.Close(); err != nil {
					t.Error(err)
				}
			}
			if native != nil {
				if err := native.Close(); err != nil {
					t.Error(err)
				}
			}
		})
	}
}

func TestFileBoundaryNativeModesSubprocess(t *testing.T) {
	if os.Getenv("SEASPRAK_STEP13_NATIVE_HELPER") != "1" {
		return
	}
	path, action := os.Getenv("SEASPRAK_STEP13_NATIVE_PATH"), os.Getenv("SEASPRAK_STEP13_NATIVE_ACTION")
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
	file, err := step13NativeAcquire(root, directory)
	if action == "native-busy" {
		if file != nil || !errors.Is(err, step13NativeBusy) {
			if file != nil {
				_ = step13NativeCloseLock(file, fileBoundaryUnlockHandle)
			}
			t.Fatal("READ|WRITE native must see old flock busy:", err)
		}
	} else if action == "native-available" {
		if err != nil {
			t.Fatal(err)
		}
		if err := step13NativeCloseLock(file, fileBoundaryUnlockHandle); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal("unknown helper action")
	}
}

func step13NativeProbe(t *testing.T, path, action string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFileBoundaryNativeModesSubprocess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SEASPRAK_STEP13_NATIVE_HELPER=1", "SEASPRAK_STEP13_NATIVE_PATH="+path, "SEASPRAK_STEP13_NATIVE_ACTION="+action)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actual cross-process %s: %v: %s", action, err, out)
	}
	t.Logf("actual cross-process %s passed", action)
}
