//go:build windows

package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"golang.org/x/sys/windows"
)

func TestSyncRootNativeValidationAndCleanup(t *testing.T) {
	for _, fault := range []string{"different-directory", "ordinary-file", "closed-new-file", "closed-root", "readonly-flush", "file-and-error"} {
		t.Run(fault, func(t *testing.T) {
			path := t.TempDir()
			root, err := os.OpenRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			var opened, directory *os.File
			calls := 0
			cause := errors.New("opener failed after opening")
			err = syncRootWithOpen(root, func(bound *os.File) (*os.File, error) {
				calls++
				directory = bound
				var openErr error
				switch fault {
				case "different-directory":
					opened, openErr = os.Open(t.TempDir())
				case "ordinary-file":
					opened, openErr = root.OpenFile("ordinary", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
				case "readonly-flush":
					opened, openErr = root.Open(".")
				default:
					opened, openErr = openSyncDirectory(bound)
				}
				if openErr != nil {
					return opened, openErr
				}
				switch fault {
				case "closed-new-file":
					if err := opened.Close(); err != nil {
						t.Fatal(err)
					}
				case "closed-root":
					if err := root.Close(); err != nil {
						t.Fatal(err)
					}
				case "file-and-error":
					return opened, cause
				}
				return opened, nil
			})
			if calls != 1 || err == nil || opened == nil || directory == nil {
				t.Fatalf("calls=%d failed validation=%v", calls, err)
			}
			for _, file := range []*os.File{opened, directory} {
				if _, statErr := file.Stat(); !errors.Is(statErr, windows.ERROR_INVALID_HANDLE) {
					t.Fatal("owned temporary handle was not closed:", statErr)
				}
			}
			switch fault {
			case "different-directory", "ordinary-file":
				var pe *product.Error
				if !errors.As(err, &pe) || pe.Code != product.CodeInvalidArgument {
					t.Fatal("wrong identity/type was not invalid_argument")
				}
			case "closed-new-file":
				if !errors.Is(err, os.ErrClosed) {
					t.Fatal("lost real double-close cause")
				}
			case "readonly-flush":
				if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
					t.Fatal("real readonly flush denial was swallowed:", err)
				}
			case "file-and-error":
				if !errors.Is(err, cause) {
					t.Fatal("lost opener cause")
				}
			}
			if fault != "closed-root" {
				if _, err := root.Stat("."); err != nil {
					t.Fatal("borrowed root was closed:", err)
				}
			}
		})
	}
}

func TestSyncRootNativeRightsSharingAndErrno(t *testing.T) {
	path := t.TempDir()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	file, err := openSyncDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := step13NativeDirectoryAttrs(t, mustSyncRootStat(t, file)); got&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		t.Fatal("native directory unexpectedly reparse")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(blocker)
	failed, nativeErr := openSyncDirectory(dir)
	if failed != nil || !errors.Is(nativeErr, windows.ERROR_SHARING_VIOLATION) {
		t.Fatal("native error was not converted to sharing errno:", nativeErr)
	}
	if pe, ok := product.AsError(SyncRoot(root)); !ok || pe.Code != product.CodeStorageUnavailable {
		t.Fatal("SyncRoot did not keep direct storage_unavailable")
	}
	if err := windows.CloseHandle(blocker); err != nil {
		t.Fatal(err)
	}
	if err := SyncRoot(root); err != nil {
		t.Fatal("sync did not recover after blocker close:", err)
	}
	if err := dir.Close(); err != nil {
		t.Fatal(err)
	}
	if file, err := openSyncDirectory(dir); file != nil || err == nil {
		if file != nil {
			_ = file.Close()
		}
		t.Fatal("native accepted closed directory")
	}
	if err := os.WriteFile(filepath.Join(path, "still-borrowed"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
}

func mustSyncRootStat(t *testing.T, file *os.File) os.FileInfo {
	t.Helper()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return info
}
