//go:build !windows

package storage

import "os"

// IsReparseInfo checks already observed metadata without reopening a path.
func IsReparseInfo(info os.FileInfo) bool {
	return info == nil || info.Mode()&os.ModeSymlink != 0
}

func IsReparse(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return info.Mode()&os.ModeSymlink != 0, nil
}
