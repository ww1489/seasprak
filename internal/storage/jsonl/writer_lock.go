package jsonl

import (
	"errors"
	"os"

	product "github.com/ww1489/seasprak/internal/errors"
)

// writerLock owns its file independently of unlocking, so an Unlock failure
// cannot abandon the remaining Close obligation.
type writerLock struct {
	file   *os.File
	locked bool
}

func (lock *writerLock) Close() error {
	return lock.closeWithUnlock(unlockWriterFile)
}

func (lock *writerLock) closeWithUnlock(unlock func(*os.File) error) error {
	if lock == nil || lock.file == nil {
		return nil
	}
	file := lock.file
	lock.file = nil
	var err error
	if lock.locked {
		err = unlock(file)
		lock.locked = false
	}
	return errors.Join(err, file.Close())
}

func openWriterLock(root *os.Root) (*writerLock, error) {
	return openWriterLockWith(root, boundOpenFile, tryWriterFileLock)
}

// Injection is limited to this call's leaf open and try-lock, so tests exercise
// the same platform retry core and owner used by the default Store.
func openWriterLockWith(root *os.Root, open func(*os.Root, string, int, os.FileMode) (*os.File, error), try func(*os.File) (bool, error)) (*writerLock, error) {
	_, err := root.Lstat("writer.lock")
	flags := os.O_RDONLY // Unix retains old flock's read-only acceptance.
	if os.IsNotExist(err) {
		flags |= os.O_CREATE | os.O_EXCL
	} else if err != nil {
		return nil, err
	}
	file, err := checkedBoundLeaf(root, "writer.lock", flags, 0600, open)
	if err != nil {
		return nil, err
	}
	lock := &writerLock{file: file}
	ok, err := tryBoundWriterLock(root, lock, open, try)
	if err != nil || !ok {
		if err == nil {
			err = product.NewError(product.CodeStateConflict, "session already has a writer")
		}
		return nil, errors.Join(err, lock.Close())
	}
	lock.locked = true
	opened, statErr := lock.file.Stat()
	current, currentErr := root.Lstat("writer.lock")
	if statErr != nil || currentErr != nil {
		if os.IsNotExist(currentErr) {
			currentErr = product.NewError(product.CodeInvalidArgument, "writer lock changed while locking")
		}
		return nil, errors.Join(statErr, currentErr, lock.Close())
	}
	if !ordinaryBoundFile(opened) || !ordinaryBoundFile(current) || !os.SameFile(opened, current) {
		return nil, errors.Join(product.NewError(product.CodeInvalidArgument, "writer lock changed while locking"), lock.Close())
	}
	return lock, nil
}
