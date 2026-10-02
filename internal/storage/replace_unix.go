//go:build !windows

package storage

import "os"

// ReplaceFile atomically replaces dst with src on the same directory.
func ReplaceFile(src, dst string) error {
	return os.Rename(src, dst)
}
