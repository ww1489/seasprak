//go:build windows

package codeagent

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// This gate characterizes only the Rename step of manifest publication. The
// legacy manifest writer uses os.Rename, not storage.ReplaceFile/WRITE_THROUGH.
// A passing characterization test does not approve a changed production method.
func TestFileBoundaryManifestRenameCompatibilityGate(t *testing.T) {
	type observation struct {
		published bool
		code      syscall.Errno
	}
	for _, kind := range []string{"missing", "ordinary", "share-delete-reader", "legacy-reader"} {
		t.Run(kind, func(t *testing.T) {
			results := make(map[string]observation)
			for _, method := range []string{"legacy", "relative"} {
				t.Run(method, func(t *testing.T) {
					dir := t.TempDir()
					root, err := os.OpenRoot(dir)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = root.Close() })
					const source, target = ".manifest-candidate", "manifest.json"
					const payload, oldPayload = "candidate manifest", "original manifest"
					file, err := root.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = file.Close() })
					if n, err := file.WriteString(payload); err != nil || n != len(payload) {
						t.Fatal("candidate write:", err)
					}
					if err := file.Sync(); err != nil {
						t.Fatal(err)
					}
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
					sourceInfo, err := root.Lstat(source)
					if err != nil {
						t.Fatal(err)
					}
					var targetInfo os.FileInfo
					if kind != "missing" {
						if err := root.WriteFile(target, []byte(oldPayload), 0600); err != nil {
							t.Fatal(err)
						}
						targetInfo, err = root.Lstat(target)
						if err != nil {
							t.Fatal(err)
						}
					}
					var held *os.File
					switch kind {
					case "share-delete-reader":
						name, err := windows.UTF16PtrFromString(filepath.Join(dir, target))
						if err != nil {
							t.Fatal(err)
						}
						handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
						if err != nil {
							t.Fatal("share-delete fixture:", err)
						}
						held = os.NewFile(uintptr(handle), target)
						if held == nil {
							_ = windows.CloseHandle(handle)
							t.Fatal("wrap fixture handle")
						}
					case "legacy-reader":
						held, err = os.Open(filepath.Join(dir, target))
						if err != nil {
							t.Fatal("legacy fixture:", err)
						}
					}
					if held != nil {
						t.Cleanup(func() { _ = held.Close() })
					}
					var renameErr error
					if method == "legacy" {
						renameErr = os.Rename(filepath.Join(dir, source), filepath.Join(dir, target))
					} else {
						renameErr = root.Rename(source, target)
					}
					got := observation{published: renameErr == nil}
					if renameErr != nil && !errors.As(renameErr, &got.code) {
						t.Fatal("rename did not return an OS errno:", renameErr)
					}
					if held != nil {
						info, statErr := held.Stat()
						data := make([]byte, len(oldPayload))
						n, readErr := held.ReadAt(data, 0)
						if statErr != nil || !os.SameFile(info, targetInfo) || readErr != nil || n != len(data) || string(data) != oldPayload {
							t.Fatal("rename changed the held original target identity or bytes")
						}
					}
					if got.published {
						if _, err := root.Lstat(source); !errors.Is(err, os.ErrNotExist) {
							t.Fatal("published source still named:", err)
						}
						info, err := root.Lstat(target)
						data, readErr := root.ReadFile(target)
						if err != nil || !os.SameFile(sourceInfo, info) || info.Mode() != sourceInfo.Mode() || readErr != nil || string(data) != payload {
							t.Fatal("publication lost original source identity or bytes")
						}
					} else {
						info, err := root.Lstat(source)
						data, readErr := root.ReadFile(source)
						if err != nil || !os.SameFile(sourceInfo, info) || info.Mode() != sourceInfo.Mode() || readErr != nil || string(data) != payload {
							t.Fatal("failed rename changed source identity or bytes")
						}
						if targetInfo != nil {
							info, err := root.Lstat(target)
							data, readErr := root.ReadFile(target)
							if err != nil || !os.SameFile(targetInfo, info) || info.Mode() != targetInfo.Mode() || readErr != nil || string(data) != oldPayload {
								t.Fatal("failed rename changed target identity or bytes")
							}
						}
					}
					if held != nil {
						if err := held.Close(); err != nil {
							t.Fatal(err)
						}
					}
					results[method] = got
					t.Logf("%s renameCalls=1 published=%v errno=%d; source/target and held-reader identity/bytes checked", method, got.published, got.code)
				})
			}
			if len(results) != 2 {
				t.Fatal("both cold methods must actually execute")
			}
			if results["legacy"] != results["relative"] {
				t.Logf("BLOCKED: relative Rename differs from legacy manifest Rename for %s: legacy=%+v relative=%+v; production method remains unchanged", kind, results["legacy"], results["relative"])
			}
		})
	}
}
