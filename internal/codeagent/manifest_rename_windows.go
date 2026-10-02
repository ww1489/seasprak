//go:build windows

package codeagent

import (
	"errors"
	"os"
	goruntime "runtime"
	"unsafe"

	store "github.com/ww1489/seasprak/internal/storage"
	"golang.org/x/sys/windows"
)

// Public FILE_RENAME_INFORMATION ABI. The first field sets only traditional
// ReplaceIfExists; no Ex/POSIX or WRITE_THROUGH method is requested.
type manifestRenameInfo struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

type manifestRenameABI struct {
	rootA [int(unsafe.Offsetof(manifestRenameInfo{}.RootDirectory)) - int(unsafe.Sizeof(uintptr(0)))]byte
	rootB [int(unsafe.Sizeof(uintptr(0))) - int(unsafe.Offsetof(manifestRenameInfo{}.RootDirectory))]byte
	lenA  [int(unsafe.Offsetof(manifestRenameInfo{}.FileNameLength)) - 2*int(unsafe.Sizeof(uintptr(0)))]byte
	lenB  [2*int(unsafe.Sizeof(uintptr(0))) - int(unsafe.Offsetof(manifestRenameInfo{}.FileNameLength))]byte
	nameA [int(unsafe.Offsetof(manifestRenameInfo{}.FileName)) - 2*int(unsafe.Sizeof(uintptr(0))) - 4]byte
	nameB [2*int(unsafe.Sizeof(uintptr(0))) + 4 - int(unsafe.Offsetof(manifestRenameInfo{}.FileName))]byte
}

func manifestRenameErrno(err error) error {
	var status windows.NTStatus
	if errors.As(err, &status) {
		return status.Errno()
	}
	return err
}

func renameManifest(root *os.Root, source string, written os.FileInfo) (bool, error) {
	return renameManifestWithDirectoryBoundary(root, source, written, nil)
}

// The optional boundary sees only this call's actual source and directory
// Files immediately before class10. Production always supplies nil.
func renameManifestWithDirectoryBoundary(root *os.Root, source string, written os.FileInfo, beforeRename func(*os.File, *os.File)) (published bool, err error) {
	before, err := root.Lstat(source)
	if err != nil {
		return false, err
	}
	if !manifestWritten(before, written) {
		return false, manifestIdentityError()
	}
	bound, err := root.Lstat(".")
	if err != nil {
		return false, err
	}
	if !bound.IsDir() || store.IsReparseInfo(bound) {
		return false, manifestIdentityError()
	}
	directory, err := root.Open(".")
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := directory.Close(); err == nil {
			err = closeErr
		}
	}()
	openedDir, err := directory.Stat()
	if err != nil {
		return false, err
	}
	currentDir, err := root.Lstat(".")
	if err != nil {
		return false, err
	}
	if !openedDir.IsDir() || store.IsReparseInfo(openedDir) || !currentDir.IsDir() || store.IsReparseInfo(currentDir) || !os.SameFile(bound, openedDir) || !os.SameFile(openedDir, currentDir) {
		return false, manifestIdentityError()
	}
	name, err := windows.NewNTUnicodeString(source)
	if err != nil {
		return false, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(directory.Fd()), ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, windows.DELETE|windows.SYNCHRONIZE|windows.FILE_READ_ATTRIBUTES, &attrs, &windows.IO_STATUS_BLOCK{}, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	goruntime.KeepAlive(name)
	goruntime.KeepAlive(directory)
	if err != nil {
		return false, manifestRenameErrno(err)
	}
	file := os.NewFile(uintptr(handle), source)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return false, os.ErrInvalid
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return false, err
	}
	current, err := root.Lstat(source)
	if err != nil {
		return false, err
	}
	if !manifestWritten(opened, written) || !manifestWritten(current, written) || !os.SameFile(before, opened) {
		return false, manifestIdentityError()
	}
	var mode uint32
	var modeStatus windows.IO_STATUS_BLOCK
	err = windows.NtQueryInformationFile(windows.Handle(file.Fd()), &modeStatus, (*byte)(unsafe.Pointer(&mode)), uint32(unsafe.Sizeof(mode)), 16) // Public FileModeInformation.
	goruntime.KeepAlive(file)
	if err != nil {
		return false, manifestRenameErrno(err)
	}
	if modeStatus.Information != unsafe.Sizeof(mode) || mode != windows.FILE_SYNCHRONOUS_IO_NONALERT {
		return false, os.ErrInvalid
	}
	if beforeRename != nil {
		beforeRename(file, directory)
	}
	leaf, err := windows.UTF16FromString("manifest.json")
	if err != nil {
		return false, err
	}
	leaf = leaf[:len(leaf)-1]
	word := unsafe.Sizeof(uintptr(0))
	size := unsafe.Sizeof(manifestRenameInfo{}) + uintptr(len(leaf))*2
	buffer := make([]uintptr, (size+word-1)/word)
	base := unsafe.Pointer(&buffer[0])
	info := (*manifestRenameInfo)(base)
	info.ReplaceIfExists = 1
	info.RootDirectory = windows.Handle(directory.Fd())
	info.FileNameLength = uint32(len(leaf) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Add(base, unsafe.Offsetof(info.FileName))), len(leaf)), leaf)
	err = windows.NtSetInformationFile(windows.Handle(file.Fd()), &windows.IO_STATUS_BLOCK{}, (*byte)(base), uint32(size), windows.FileRenameInformation)
	goruntime.KeepAlive(buffer)
	goruntime.KeepAlive(file)
	goruntime.KeepAlive(directory)
	if err != nil {
		return false, manifestRenameErrno(err)
	}
	final, err := root.Lstat("manifest.json")
	if err != nil {
		return true, err
	}
	if !manifestWritten(final, written) {
		return true, manifestIdentityError()
	}
	return true, nil
}
