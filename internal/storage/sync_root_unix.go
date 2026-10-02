//go:build !windows

package storage

import (
	"errors"
	"os"

	product "github.com/ww1489/seasprak/internal/errors"
)

// SyncRoot flushes the bound directory without reopening its diagnostic path.
// The root is borrowed and remains owned by the caller.
func SyncRoot(root *os.Root) error {
	if root == nil {
		return product.NewError(product.CodeInvalidArgument, "directory root is required")
	}
	file, err := root.Open(".")
	if err != nil {
		return product.NewError(product.CodeStorageUnavailable, "directory sync failed")
	}
	err = errors.Join(file.Sync(), file.Close())
	if err != nil {
		return product.NewError(product.CodeStorageUnavailable, "directory sync failed")
	}
	return nil
}
