//go:build !windows

package codeagent

import "os"

// Unix retains its ordinary, overwriting rename. The observed identity checks
// are not an atomic expected-source-ID condition on the rename syscall.
func renameManifest(root *os.Root, source string, written os.FileInfo) (bool, error) {
	before, err := root.Lstat(source)
	if err != nil {
		return false, err
	}
	if !manifestWritten(before, written) {
		return false, manifestIdentityError()
	}
	if err := root.Rename(source, "manifest.json"); err != nil {
		return false, err
	}
	final, err := root.Lstat("manifest.json")
	if err != nil {
		return true, err
	}
	if !manifestWritten(final, written) {
		return true, manifestIdentityError()
	}
	return true, nil
}
