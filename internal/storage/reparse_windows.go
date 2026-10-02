//go:build windows

package storage

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// IsReparseInfo checks the complete reparse attribute on observed metadata.
// Unknown Sys values fail closed; no path-based lookup is performed.
func IsReparseInfo(info os.FileInfo) bool {
	if info == nil {
		return true
	}
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return !ok || attrs == nil || attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func IsReparse(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return true, nil
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return false, err
	}
	return attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}
