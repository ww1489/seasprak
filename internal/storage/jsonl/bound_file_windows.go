//go:build windows

package jsonl

import (
	"errors"
	"os"
	"unsafe"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"golang.org/x/sys/windows"
)

// Only these two fixed leaves use the native opener. It preserves the old
// READ/WRITE rights and denies DELETE sharing, unlike Root.OpenFile on Windows.
func boundOpenFile(root *os.Root, name string, flags int, mode os.FileMode) (file *os.File, err error) {
	if name != "journal.jsonl" && name != "writer.lock" || flags&os.O_TRUNC != 0 || flags&os.O_CREATE != 0 && flags&os.O_EXCL == 0 {
		return nil, product.NewError(product.CodeInvalidArgument, "journal or writer lock open is invalid")
	}
	before, err := root.Lstat(".")
	if err != nil {
		return nil, err
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, directory.Close())
		if err != nil && file != nil {
			err = errors.Join(err, file.Close())
			file = nil
		}
	}()
	borrowed, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || store.IsReparseInfo(before) || !borrowed.IsDir() || store.IsReparseInfo(borrowed) || !os.SameFile(before, borrowed) {
		return nil, product.NewError(product.CodeInvalidArgument, "journal directory has invalid type or identity")
	}
	leaf, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attrs := &windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(directory.Fd()), ObjectName: leaf, Attributes: windows.OBJ_DONT_REPARSE | windows.OBJ_CASE_INSENSITIVE}
	attrs.Length = uint32(unsafe.Sizeof(*attrs))
	access := uint32(windows.FILE_GENERIC_READ)
	if name == "writer.lock" || flags&os.O_RDWR != 0 {
		access |= windows.FILE_GENERIC_WRITE
	}
	disposition := uint32(windows.FILE_OPEN)
	if flags&os.O_CREATE != 0 {
		disposition = windows.FILE_CREATE
	}
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, access, attrs, &windows.IO_STATUS_BLOCK{}, nil, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, disposition,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		var status windows.NTStatus
		if errors.As(err, &status) {
			err = status.Errno()
		}
		return nil, err
	}
	file = os.NewFile(uintptr(handle), name)
	if file == nil {
		return nil, errors.Join(os.ErrInvalid, windows.CloseHandle(handle))
	}
	return file, nil
}
