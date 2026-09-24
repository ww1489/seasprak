//go:build windows

package store

import "golang.org/x/sys/windows"

// ReplaceFile replaces dst with src using MoveFileEx.
// REPLACE_EXISTING allows the journal name to stay stable.
// WRITE_THROUGH flushes the replaced file; it is not a promise for every volume.
func ReplaceFile(src, dst string) error {
	from, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
