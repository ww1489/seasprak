//go:build windows

package jsonl

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryWriterFileLock(file *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func unlockWriterFile(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
}

func tryBoundWriterLock(root *os.Root, lock *writerLock, open func(*os.Root, string, int, os.FileMode) (*os.File, error), try func(*os.File) (bool, error)) (bool, error) {
	return try(lock.file)
}
