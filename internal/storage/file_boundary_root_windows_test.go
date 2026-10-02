//go:build windows

package storage

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Sys comes from Root.Lstat's relative handle, never a path-based reopen.
func fileBoundaryReparseInfo(info os.FileInfo) bool {
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return !ok || attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func TestFileBoundaryWindowsRelativeDirectorySyncPrototype(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	path := filepath.Join(state, "flush")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	trusted, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	defer trusted.Close()
	bound, err := fileBoundaryOpenCheckedRoot(trusted, "flush")
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("external"), 0644); err != nil {
		t.Fatal(err)
	}
	before := fileBoundaryTree(t, outside)
	readHandle, err := bound.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer readHandle.Close()
	readInfo, err := readHandle.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	storageDirectoryLink(t, path, outside)
	t.Cleanup(func() { _ = os.Remove(path) })
	// Root.OpenFile requests FILE_NON_DIRECTORY_FILE for O_WRONLY/O_RDWR.
	// As in Go's OBJECT_ATTRIBUTES.init, an empty native name opens the retained
	// directory itself. No absolute path, Root.Name(), or ReOpenFile is used.
	name, err := windows.NewNTUnicodeString("")
	if err != nil {
		t.Fatal(err)
	}
	attrs := &windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(readHandle.Fd()), ObjectName: name, Attributes: windows.OBJ_DONT_REPARSE}
	attrs.Length = uint32(unsafe.Sizeof(*attrs))
	var h windows.Handle
	err = windows.NtCreateFile(&h, windows.FILE_GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES, attrs, &windows.IO_STATUS_BLOCK{}, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		t.Fatal("relative NtCreateFile directory write access:", err)
	}
	writeHandle := os.NewFile(uintptr(h), "step12-bound-directory")
	if writeHandle == nil {
		_ = windows.CloseHandle(h)
		t.Fatal("wrap reopened directory")
	}
	defer writeHandle.Close()
	writeInfo, err := writeHandle.Stat()
	if err != nil || !os.SameFile(readInfo, writeInfo) {
		t.Fatal("relative directory sync changed identity", err)
	}
	flushErr := windows.FlushFileBuffers(windows.Handle(writeHandle.Fd()))
	if flushErr != nil && !errors.Is(flushErr, windows.ERROR_INVALID_FUNCTION) {
		t.Fatal("relative directory flush:", flushErr)
	}
	t.Logf("GENERIC_WRITE same-object directory flush result=%v", flushErr)
	if err := writeHandle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := readHandle.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
		t.Error("relative directory flush changed external bytes, modes or entries")
	}
}

func TestFileBoundaryWindowsSyncDirWriteAccessAndErrors(t *testing.T) {
	dir := t.TempDir()
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	// The held read handle refuses WRITE sharing, so the real GENERIC_WRITE
	// directory flush opener must report its sharing error, not swallow it.
	if err := SyncDir(dir); err != windows.ERROR_SHARING_VIOLATION {
		t.Fatalf("directory sync sharing failure=%v, want ERROR_SHARING_VIOLATION", err)
	}
	if err := windows.FlushFileBuffers(h); err != windows.ERROR_ACCESS_DENIED {
		t.Fatalf("read-only directory flush=%v, want ERROR_ACCESS_DENIED", err)
	}
}
