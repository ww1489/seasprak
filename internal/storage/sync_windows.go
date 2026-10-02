//go:build windows

package storage

import (
	"errors"

	"golang.org/x/sys/windows"
)

// SyncDir flushes a directory after a file create or replace.
// The handle is opened with FILE_FLAG_BACKUP_SEMANTICS and GENERIC_WRITE.
// A read-only directory handle returns ERROR_ACCESS_DENIED from FlushFileBuffers,
// so that open mode is not used. NTFS may still return ERROR_INVALID_FUNCTION
// because directory flush is not implemented; only that error is treated as a
// platform limitation. File payload durability still depends on File.Sync.
// MoveFileEx WRITE_THROUGH covers replacement and is not a promise for every volume.
func SyncDir(dir string) error {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	err = windows.FlushFileBuffers(h)
	if err == nil || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
		return nil
	}
	return err
}
