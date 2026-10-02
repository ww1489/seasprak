//go:build windows

package storage

import (
	"errors"
	"os"
	"unsafe"

	product "github.com/ww1489/seasprak/internal/errors"
	"golang.org/x/sys/windows"
)

// SyncRoot flushes the bound directory through an empty-name native open.
// The root is borrowed. This is not a replacement for WRITE_THROUGH publication.
func SyncRoot(root *os.Root) error {
	err := syncRootWithOpen(root, openSyncDirectory)
	if err == nil {
		return nil
	}
	var pe *product.Error
	if errors.As(err, &pe) && pe.Code == product.CodeInvalidArgument {
		return product.NewError(product.CodeInvalidArgument, "directory root has invalid type or identity")
	}
	return product.NewError(product.CodeStorageUnavailable, "directory sync failed")
}

// The opener is per call and only accepts the retained directory file.
// All temporary files are closed even when validation or flush fails.
func syncRootWithOpen(root *os.Root, open func(*os.File) (*os.File, error)) (err error) {
	invalid := func() error {
		return product.NewError(product.CodeInvalidArgument, "directory root has invalid type or identity")
	}
	if root == nil {
		return invalid()
	}
	before, err := root.Lstat(".")
	if err != nil {
		return err
	}
	if !before.IsDir() || IsReparseInfo(before) {
		return invalid()
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	borrowed, err := directory.Stat()
	if err != nil {
		return err
	}
	if !borrowed.IsDir() || IsReparseInfo(borrowed) || !os.SameFile(before, borrowed) {
		return invalid()
	}
	file, err := open(directory)
	if file != nil {
		defer func() { err = errors.Join(err, file.Close()) }()
	}
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.IsDir() || IsReparseInfo(opened) || !os.SameFile(before, opened) {
		return invalid()
	}
	current, err := root.Lstat(".")
	if err != nil {
		return err
	}
	if !current.IsDir() || IsReparseInfo(current) || !os.SameFile(opened, current) {
		return invalid()
	}
	err = windows.FlushFileBuffers(windows.Handle(file.Fd()))
	if errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
		return nil // Only this actual flush error is a platform exception.
	}
	return err
}

func openSyncDirectory(directory *os.File) (*os.File, error) {
	name, err := windows.NewNTUnicodeString("")
	if err != nil {
		return nil, err
	}
	attrs := &windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(directory.Fd()), ObjectName: name, Attributes: windows.OBJ_DONT_REPARSE | windows.OBJ_CASE_INSENSITIVE}
	attrs.Length = uint32(unsafe.Sizeof(*attrs))
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, windows.FILE_GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES, attrs, &windows.IO_STATUS_BLOCK{}, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		var status windows.NTStatus
		if errors.As(err, &status) {
			err = status.Errno()
		}
		return nil, err
	}
	file := os.NewFile(uintptr(handle), ".")
	if file == nil {
		return nil, errors.Join(os.ErrInvalid, windows.CloseHandle(handle))
	}
	return file, nil
}
