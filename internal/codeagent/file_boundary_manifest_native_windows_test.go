//go:build windows

package codeagent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"syscall"
	"testing"
	"unsafe"

	store "github.com/ww1489/seasprak/internal/storage"
	"golang.org/x/sys/windows"
)

// This is a test-only traditional rename experiment for manifest publication.
// The legacy manifest writer has no WRITE_THROUGH request. None of this file
// changes that writer, Root.Rename, Repair, ReplaceFile or journal/lock sharing.
type manifestGateRenameInfo struct {
	ReplaceOrFlags uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

type manifestGateCalls struct {
	open, rename, close int
	mode                uint32
}

// Static public-ABI offset checks for the compiling HANDLE width.
type manifestGateABI struct {
	rootA [int(unsafe.Offsetof(manifestGateRenameInfo{}.RootDirectory)) - int(unsafe.Sizeof(uintptr(0)))]byte
	rootB [int(unsafe.Sizeof(uintptr(0))) - int(unsafe.Offsetof(manifestGateRenameInfo{}.RootDirectory))]byte
	lenA  [int(unsafe.Offsetof(manifestGateRenameInfo{}.FileNameLength)) - 2*int(unsafe.Sizeof(uintptr(0)))]byte
	lenB  [2*int(unsafe.Sizeof(uintptr(0))) - int(unsafe.Offsetof(manifestGateRenameInfo{}.FileNameLength))]byte
	nameA [int(unsafe.Offsetof(manifestGateRenameInfo{}.FileName)) - 2*int(unsafe.Sizeof(uintptr(0))) - 4]byte
	nameB [2*int(unsafe.Sizeof(uintptr(0))) + 4 - int(unsafe.Offsetof(manifestGateRenameInfo{}.FileName))]byte
}

func manifestGateErrno(err error) error {
	var status windows.NTStatus
	if errors.As(err, &status) {
		return status.Errno()
	}
	return err
}

func manifestGateRename(root *os.Root, source string, calls *manifestGateCalls) error {
	return manifestGateRenameWithDirectoryBoundary(root, source, calls, nil)
}

// This test-only boundary runs after obtaining the real native source and
// immediately before the same traditional rename. It never substitutes I/O.
func manifestGateRenameWithDirectoryBoundary(root *os.Root, source string, calls *manifestGateCalls, beforeRename func(*os.File, *os.File)) (err error) {
	before, err := root.Lstat(source)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || store.IsReparseInfo(before) {
		return os.ErrInvalid
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	bound, err := root.Stat(".")
	if err != nil {
		return err
	}
	openedDir, err := directory.Stat()
	if err != nil || !openedDir.IsDir() || store.IsReparseInfo(openedDir) || !os.SameFile(bound, openedDir) {
		return os.ErrInvalid
	}
	name, err := windows.NewNTUnicodeString(source)
	if err != nil {
		return err
	}
	attrs := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(directory.Fd()), ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var handle windows.Handle
	calls.open++
	err = windows.NtCreateFile(&handle, windows.DELETE|windows.SYNCHRONIZE|windows.FILE_READ_ATTRIBUTES, &attrs, &windows.IO_STATUS_BLOCK{}, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	goruntime.KeepAlive(name)
	goruntime.KeepAlive(directory)
	if err != nil {
		return manifestGateErrno(err)
	}
	file := os.NewFile(uintptr(handle), source)
	if file == nil {
		return errors.Join(os.ErrInvalid, windows.CloseHandle(handle))
	}
	defer func() {
		calls.close++
		err = errors.Join(err, file.Close())
	}()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := root.Lstat(source)
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || store.IsReparseInfo(opened) || !current.Mode().IsRegular() || store.IsReparseInfo(current) || !os.SameFile(before, opened) || !os.SameFile(opened, current) {
		return os.ErrInvalid
	}
	var mode uint32
	var modeStatus windows.IO_STATUS_BLOCK
	err = windows.NtQueryInformationFile(windows.Handle(file.Fd()), &modeStatus, (*byte)(unsafe.Pointer(&mode)), uint32(unsafe.Sizeof(mode)), 16) // Public FileModeInformation.
	goruntime.KeepAlive(file)
	if err != nil {
		return manifestGateErrno(err)
	}
	calls.mode = mode
	if modeStatus.Information != unsafe.Sizeof(mode) || mode&windows.FILE_SYNCHRONOUS_IO_NONALERT == 0 || mode&windows.FILE_WRITE_THROUGH != 0 {
		return os.ErrInvalid
	}
	if beforeRename != nil {
		beforeRename(file, directory)
	}
	// Public ntifs.h ABI, not Go's private fixed-MAX_PATH structure. The
	// buffer is HANDLE aligned; FileNameLength excludes the terminating NUL.
	leaf, err := windows.UTF16FromString("manifest.json")
	if err != nil {
		return err
	}
	leaf = leaf[:len(leaf)-1]
	word := unsafe.Sizeof(uintptr(0))
	size := unsafe.Sizeof(manifestGateRenameInfo{}) + uintptr(len(leaf))*2
	buffer := make([]uintptr, (size+word-1)/word)
	base := unsafe.Pointer(&buffer[0])
	info := (*manifestGateRenameInfo)(base)
	info.ReplaceOrFlags = 1
	info.RootDirectory = windows.Handle(directory.Fd())
	info.FileNameLength = uint32(len(leaf) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Add(base, unsafe.Offsetof(info.FileName))), len(leaf)), leaf)
	calls.rename++
	err = windows.NtSetInformationFile(windows.Handle(file.Fd()), &windows.IO_STATUS_BLOCK{}, (*byte)(base), uint32(size), windows.FileRenameInformation)
	goruntime.KeepAlive(buffer)
	goruntime.KeepAlive(file)
	goruntime.KeepAlive(directory)
	return manifestGateErrno(err)
}

func TestFileBoundaryManifestNativeGateCompatibility(t *testing.T) {
	type observation struct {
		published bool
		code      syscall.Errno
	}
	for _, kind := range []string{"missing", "ordinary", "target-share-delete-reader", "target-legacy-reader", "target-write-holder", "target-readonly", "target-directory", "source-readonly", "source-legacy-reader", "source-delete-holder", "case-alias"} {
		t.Run(kind, func(t *testing.T) {
			results := map[string]observation{}
			for _, method := range []string{"legacy", "traditional"} {
				t.Run(method, func(t *testing.T) {
					dir := t.TempDir()
					root, err := os.OpenRoot(dir)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = root.Close() })
					const source, target = ".manifest-candidate", "manifest.json"
					const payload, oldPayload = "candidate manifest", "original manifest"
					actualSource := source
					if kind == "case-alias" {
						actualSource = ".MANIFEST-CANDIDATE"
					}
					if err := root.WriteFile(actualSource, []byte(payload), 0600); err != nil {
						t.Fatal(err)
					}
					if kind == "source-readonly" {
						if err := root.Chmod(source, 0400); err != nil {
							t.Fatal(err)
						}
					}
					if kind == "target-directory" {
						if err := root.Mkdir(target, 0700); err != nil {
							t.Fatal(err)
						}
						if err := root.WriteFile(target+"/sentinel", []byte(oldPayload), 0600); err != nil {
							t.Fatal(err)
						}
					} else if kind != "missing" {
						if err := root.WriteFile(target, []byte(oldPayload), 0600); err != nil {
							t.Fatal(err)
						}
						if kind == "target-readonly" {
							if err := root.Chmod(target, 0400); err != nil {
								t.Fatal(err)
							}
						}
					}
					sourceInfo, err := root.Lstat(source)
					if err != nil {
						t.Fatal(err)
					}
					var targetInfo os.FileInfo
					if kind != "missing" {
						targetInfo, err = root.Lstat(target)
						if err != nil {
							t.Fatal(err)
						}
					}
					var held *os.File
					switch kind {
					case "target-legacy-reader", "source-legacy-reader":
						leaf := target
						if kind == "source-legacy-reader" {
							leaf = source
						}
						held, err = os.Open(filepath.Join(dir, leaf))
					case "target-share-delete-reader", "target-write-holder", "source-delete-holder":
						leaf, access, share := target, uint32(windows.GENERIC_READ), uint32(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
						if kind == "target-write-holder" {
							access, share = windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE
						} else if kind == "source-delete-holder" {
							leaf, access = source, windows.DELETE|windows.SYNCHRONIZE|windows.FILE_READ_ATTRIBUTES
						}
						path, nameErr := windows.UTF16PtrFromString(filepath.Join(dir, leaf))
						if nameErr != nil {
							t.Fatal(nameErr)
						}
						handle, openErr := windows.CreateFile(path, access, share, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
						err = openErr
						if err == nil {
							held = os.NewFile(uintptr(handle), leaf)
							if held == nil {
								_ = windows.CloseHandle(handle)
								t.Fatal("wrap holder")
							}
						}
					}
					if err != nil {
						t.Fatal("holder fixture:", err)
					}
					if held != nil {
						t.Cleanup(func() { _ = held.Close() })
					}
					calls := manifestGateCalls{}
					var renameErr error
					if method == "legacy" {
						renameErr = os.Rename(filepath.Join(dir, source), filepath.Join(dir, target))
					} else {
						renameErr = manifestGateRename(root, source, &calls)
						wantRename := 1
						if kind == "source-legacy-reader" {
							wantRename = 0
						}
						if calls.open != 1 || calls.rename != wantRename {
							t.Fatalf("native source opens/rename calls=%d/%d, want 1/%d", calls.open, calls.rename, wantRename)
						}
					}
					got := observation{published: renameErr == nil}
					if renameErr != nil && !errors.As(renameErr, &got.code) {
						t.Fatal("rename has no OS errno:", renameErr)
					}
					if held != nil {
						info, err := held.Stat()
						want := targetInfo
						if kind == "source-legacy-reader" || kind == "source-delete-holder" {
							want = sourceInfo
						}
						if err != nil || !os.SameFile(want, info) {
							t.Fatal("holder identity changed:", err)
						}
						if err := held.Close(); err != nil {
							t.Fatal(err)
						}
					}
					if got.published {
						info, err := root.Lstat(target)
						data, readErr := root.ReadFile(target)
						if err != nil || !os.SameFile(sourceInfo, info) || info.Mode() != sourceInfo.Mode() || readErr != nil || string(data) != payload {
							t.Fatal("success did not preserve source identity, mode and bytes")
						}
						if _, err := root.Lstat(source); !os.IsNotExist(err) {
							t.Fatal("published source still named:", err)
						}
					} else {
						info, err := root.Lstat(source)
						data, readErr := root.ReadFile(source)
						if err != nil || !os.SameFile(sourceInfo, info) || info.Mode() != sourceInfo.Mode() || readErr != nil || string(data) != payload {
							t.Fatal("failed rename changed source")
						}
						if targetInfo != nil {
							leaf := target
							if kind == "target-directory" {
								leaf += "/sentinel"
							}
							info, err := root.Lstat(target)
							data, readErr := root.ReadFile(leaf)
							if err != nil || !os.SameFile(targetInfo, info) || info.Mode() != targetInfo.Mode() || readErr != nil || string(data) != oldPayload {
								t.Fatal("failed rename changed target")
							}
						}
					}
					results[method] = got
					t.Logf("%s published=%v errno=%d nativeOpen/rename=%d/%d mode=%#x; original source/target identity, mode, bytes checked", method, got.published, got.code, calls.open, calls.rename, calls.mode)
				})
			}
			if len(results) != 2 || results["legacy"] != results["traditional"] {
				t.Fatalf("BLOCKED: cold legacy/traditional observations differ: %v", results)
			}
		})
	}
}

func TestFileBoundaryManifestNativeGateBoundDirectory(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	roots, err := store.OpenResourceRoots(state, store.ResourceCode, "manifest-gate", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	for _, leaf := range []string{".manifest-candidate", "manifest.json", "sentinel"} {
		if err := os.WriteFile(filepath.Join(outside, leaf), []byte("outside unchanged"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	before := fileBoundaryTree(t, outside)
	if err := os.Rename(roots.Path, roots.Path+".held"); err != nil {
		t.Fatal("move bound directory:", err)
	}
	command, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Fatal("junction command unavailable:", err)
	}
	if output, err := exec.Command(command, "/d", "/c", "mklink", "/J", roots.Path, outside).CombinedOutput(); err != nil {
		t.Fatalf("junction fixture: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = os.Remove(roots.Path) })
	if err := roots.Resource.WriteFile(".manifest-candidate", []byte("bound manifest"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := roots.Resource.Lstat(".manifest-candidate")
	if err != nil {
		t.Fatal(err)
	}
	calls := manifestGateCalls{}
	if err := manifestGateRename(roots.Resource, ".manifest-candidate", &calls); err != nil {
		t.Fatal("traditional bound rename:", err)
	}
	data, err := roots.Resource.ReadFile("manifest.json")
	published, statErr := roots.Resource.Lstat("manifest.json")
	if err != nil || statErr != nil || string(data) != "bound manifest" || !os.SameFile(source, published) || calls.open != 1 || calls.rename != 1 || calls.mode != windows.FILE_SYNCHRONOUS_IO_NONALERT {
		t.Fatal("bound publication changed source binding or call/mode proof")
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
		t.Fatal("bound publication changed outside bytes, modes or entries")
	}
	if _, err := roots.Resource.Stat("."); err != nil {
		t.Fatal("test method closed borrowed root:", err)
	}
}

func TestFileBoundaryManifestNativeGateRejectsSourceAndClosedRoot(t *testing.T) {
	for _, kind := range []string{"directory", "file-symlink", "closed-root"} {
		t.Run(kind, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := os.WriteFile(filepath.Join(outside, "original"), []byte("outside source"), 0644); err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, outside)
			switch kind {
			case "directory":
				if err := root.Mkdir(".manifest-candidate", 0700); err != nil {
					t.Fatal(err)
				}
			case "file-symlink":
				fileBoundaryFileLink(t, filepath.Join(dir, ".manifest-candidate"), filepath.Join(outside, "original"))
			case "closed-root":
				if err := root.WriteFile(".manifest-candidate", []byte("original source"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}
			}
			calls := manifestGateCalls{}
			if err := manifestGateRename(root, ".manifest-candidate", &calls); err == nil || calls.open != 0 || calls.rename != 0 {
				t.Fatalf("invalid source was published: error=%v calls=%+v", err, calls)
			}
			if _, err := os.Lstat(filepath.Join(dir, "manifest.json")); !os.IsNotExist(err) {
				t.Fatal("rejected source created manifest:", err)
			}
			if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
				t.Fatal("rejected source changed outside bytes, modes or entries")
			}
		})
	}
}

func TestFileBoundaryManifestNativeGateClosedDirectoryReleasesSource(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for leaf, data := range map[string]string{".manifest-candidate": "original source", "manifest.json": "original target"} {
		if err := root.WriteFile(leaf, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sourceInfo, err := root.Lstat(".manifest-candidate")
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := root.Lstat("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	borrowedInfo, err := root.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	var captured *os.File
	var nativeHandle windows.Handle
	boundaries := 0
	calls := manifestGateCalls{}
	renameErr := manifestGateRenameWithDirectoryBoundary(root, ".manifest-candidate", &calls, func(source, directory *os.File) {
		boundaries++
		captured = source
		nativeHandle = windows.Handle(source.Fd())
		if err := directory.Close(); err != nil {
			t.Fatal("close the actual rename directory:", err)
		}
	})
	var code syscall.Errno
	if !errors.As(renameErr, &code) || code != windows.ERROR_INVALID_HANDLE || calls.open != 1 || calls.rename != 1 || calls.close != 1 || boundaries != 1 || calls.mode != windows.FILE_SYNCHRONOUS_IO_NONALERT {
		t.Fatalf("closed directory did not reach the real traditional failure: error=%v calls=%+v boundaries=%d", renameErr, calls, boundaries)
	}
	// Query the exact original source HANDLE before any other file opens can
	// reuse its numeric value. This proves kernel release, not share7 rename.
	var mode uint32
	queryErr := windows.NtQueryInformationFile(nativeHandle, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&mode)), uint32(unsafe.Sizeof(mode)), 16)
	if !errors.Is(manifestGateErrno(queryErr), windows.ERROR_INVALID_HANDLE) {
		t.Fatal("owned native source HANDLE remained queryable:", queryErr)
	}
	if captured == nil || !errors.Is(captured.Close(), os.ErrClosed) {
		t.Fatal("source File ownership was not closed")
	}
	for leaf, want := range map[string]struct {
		info os.FileInfo
		data string
	}{".manifest-candidate": {sourceInfo, "original source"}, "manifest.json": {targetInfo, "original target"}} {
		info, err := root.Lstat(leaf)
		data, readErr := root.ReadFile(leaf)
		if err != nil || readErr != nil || !os.SameFile(want.info, info) || want.info.Mode() != info.Mode() || string(data) != want.data {
			t.Fatal("failed native rename changed source or target:", leaf, err, readErr)
		}
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(borrowedInfo, after) {
		t.Fatal("failure closed or changed borrowed Root:", err)
	}
}
