//go:build !windows

package storage

import "os"

// SyncDir flushes directory metadata after a file create or replace.
// A successful file Sync does not by itself make the directory entry durable.
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
