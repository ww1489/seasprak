//go:build !windows

package jsonl

import (
	"errors"
	"os"

	product "github.com/ww1489/seasprak/internal/errors"
	"golang.org/x/sys/unix"
)

func tryWriterFileLock(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func unlockWriterFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

func tryBoundWriterLock(root *os.Root, lock *writerLock, open func(*os.Root, string, int, os.FileMode) (*os.File, error), try func(*os.File) (bool, error)) (bool, error) {
	ok, err := try(lock.file)
	if ok || err == nil || !errors.Is(err, unix.EIO) && !errors.Is(err, unix.EBADF) {
		return ok, err
	}
	// Preserve flock's NFS retry eligibility, with at most one relative RW
	// reopen. Stat failure or missing 0600 permissions never grants WRITE.
	before, statErr := lock.file.Stat()
	if statErr != nil || before.Mode()&0600 != 0600 {
		return false, err
	}
	file, openErr := checkedBoundLeaf(root, "writer.lock", os.O_RDWR, 0600, open)
	if openErr != nil {
		return false, openErr
	}
	opened, statErr := file.Stat()
	if statErr != nil {
		return false, errors.Join(statErr, file.Close())
	}
	if !ordinaryBoundFile(opened) || !os.SameFile(before, opened) {
		return false, errors.Join(product.NewError(product.CodeInvalidArgument, "writer lock changed while reopening"), file.Close())
	}
	old := lock.file
	lock.file = file
	if err := old.Close(); err != nil {
		return false, err
	}
	return try(file) // No recursive retry, even for a second EIO/EBADF.
}
