//go:build windows

package codeagent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"unsafe"

	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
	"golang.org/x/sys/windows"
)

func manifestPublishDirectoryLink(t *testing.T, link, target string) {
	t.Helper()
	command, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Fatal("junction command unavailable:", err)
	}
	if output, err := exec.Command(command, "/d", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("junction fixture: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}

// These tests invoke the production helper, not the historical test-only gate.
func TestFileBoundaryManifestPublishNativeCompatibility(t *testing.T) {
	for _, kind := range []string{"missing", "overwrite", "target-read-share7", "target-legacy-reader", "source-delete-share7", "source-no-delete"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			const source = ".manifest-production-candidate"
			file, err := root.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if n, err := file.WriteString("actual synchronized source"); err != nil || n != len("actual synchronized source") {
				t.Fatal(err)
			}
			if err := file.Sync(); err != nil {
				t.Fatal(err)
			}
			if err := file.Chmod(0600); err != nil {
				t.Fatal(err)
			}
			written, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			var target os.FileInfo
			if kind != "missing" {
				if err := root.WriteFile("manifest.json", []byte("old target"), 0600); err != nil {
					t.Fatal(err)
				}
				target, err = root.Lstat("manifest.json")
				if err != nil {
					t.Fatal(err)
				}
			}
			var holder *os.File
			switch kind {
			case "target-read-share7", "source-delete-share7":
				leaf, access := "manifest.json", uint32(windows.GENERIC_READ)
				if kind == "source-delete-share7" {
					leaf, access = source, windows.DELETE|windows.SYNCHRONIZE|windows.FILE_READ_ATTRIBUTES
				}
				name, err := windows.UTF16PtrFromString(filepath.Join(dir, leaf))
				if err != nil {
					t.Fatal(err)
				}
				handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
				if err != nil {
					t.Fatal("holder fixture:", err)
				}
				holder = os.NewFile(uintptr(handle), leaf)
				if holder == nil {
					_ = windows.CloseHandle(handle)
					t.Fatal("wrap holder")
				}
			case "target-legacy-reader", "source-no-delete":
				leaf := "manifest.json"
				if kind == "source-no-delete" {
					leaf = source
				}
				holder, err = os.Open(filepath.Join(dir, leaf))
				if err != nil {
					t.Fatal(err)
				}
			}
			if holder != nil {
				defer holder.Close()
			}
			boundaries := 0
			var mode uint32
			published, renameErr := renameManifestWithDirectoryBoundary(root, source, written, func(actual, directory *os.File) {
				boundaries++
				info, err := actual.Stat()
				if err != nil || !manifestWritten(info, written) {
					t.Fatal("native source differs from written file:", err)
				}
				var status windows.IO_STATUS_BLOCK
				err = windows.NtQueryInformationFile(windows.Handle(actual.Fd()), &status, (*byte)(unsafe.Pointer(&mode)), uint32(unsafe.Sizeof(mode)), 16)
				if err != nil || status.Information != unsafe.Sizeof(mode) || mode != 0x20 {
					t.Fatalf("actual source mode=%#x query=%v", mode, err)
				}
				info, err = directory.Stat()
				bound, boundErr := root.Stat(".")
				if err != nil || boundErr != nil || !os.SameFile(info, bound) {
					t.Fatal("native rename lost actual directory")
				}
			})
			wantCode := syscall.Errno(0)
			if kind == "target-read-share7" || kind == "target-legacy-reader" {
				wantCode = windows.ERROR_ACCESS_DENIED
			}
			if kind == "source-no-delete" {
				wantCode = windows.ERROR_SHARING_VIOLATION
			}
			var code syscall.Errno
			if renameErr != nil && !errors.As(renameErr, &code) {
				t.Fatal("non-OS native error:", renameErr)
			}
			if code != wantCode || published != (wantCode == 0) {
				t.Fatalf("published=%v errno=%d want=%d error=%v", published, code, wantCode, renameErr)
			}
			wantBoundary := 1
			if kind == "source-no-delete" {
				wantBoundary = 0
			}
			if boundaries != wantBoundary {
				t.Fatal("native class10 boundary calls:", boundaries)
			}
			if holder != nil {
				want := target
				if kind == "source-delete-share7" || kind == "source-no-delete" {
					want = written
				}
				info, err := holder.Stat()
				if err != nil || !os.SameFile(info, want) {
					t.Fatal("held identity changed:", err)
				}
				if err := holder.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if published {
				final, err := root.Lstat("manifest.json")
				raw, readErr := root.ReadFile("manifest.json")
				if err != nil || readErr != nil || !manifestWritten(final, written) || final.Mode() != written.Mode() || string(raw) != "actual synchronized source" {
					t.Fatal("native publication changed identity/mode/bytes")
				}
				if _, err := root.Lstat(source); !os.IsNotExist(err) {
					t.Fatal("source remained after publish:", err)
				}
			} else {
				info, err := root.Lstat(source)
				raw, readErr := root.ReadFile(source)
				if err != nil || readErr != nil || !manifestWritten(info, written) || info.Mode() != written.Mode() || string(raw) != "actual synchronized source" {
					t.Fatal("native failure changed source")
				}
				info, err = root.Lstat("manifest.json")
				raw, readErr = root.ReadFile("manifest.json")
				if err != nil || readErr != nil || !os.SameFile(target, info) || info.Mode() != target.Mode() || string(raw) != "old target" {
					t.Fatal("native failure changed target")
				}
			}
			t.Logf("production traditional rename published=%v errno=%d before-class10=%d actual source mode=%#x", published, code, boundaries, mode)
		})
	}
}

func TestFileBoundaryManifestPublishNativeClosedDirectoryReleasesSource(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile(".manifest-source", []byte("source bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("manifest.json", []byte("target bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	written, err := root.Lstat(".manifest-source")
	if err != nil {
		t.Fatal(err)
	}
	before := fileBoundaryTree(t, root.Name())
	targetInfo, err := root.Lstat("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	borrowedInfo, err := root.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	var source, directory *os.File
	var handle windows.Handle
	boundaries := 0
	published, renameErr := renameManifestWithDirectoryBoundary(root, ".manifest-source", written, func(actual, dir *os.File) {
		boundaries++
		source, directory, handle = actual, dir, windows.Handle(actual.Fd())
		if err := dir.Close(); err != nil {
			t.Fatal("close actual directory:", err)
		}
	})
	if published || !errors.Is(renameErr, windows.ERROR_INVALID_HANDLE) || boundaries != 1 {
		t.Fatal("closed actual directory did not produce errno6:", renameErr, boundaries)
	}
	// Query the exact original numeric HANDLE before any new open may reuse it.
	var mode uint32
	queryErr := windows.NtQueryInformationFile(handle, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&mode)), uint32(unsafe.Sizeof(mode)), 16)
	if !errors.Is(manifestRenameErrno(queryErr), windows.ERROR_INVALID_HANDLE) {
		t.Fatal("owned native source kernel handle leaked:", queryErr)
	}
	if !errors.Is(source.Close(), os.ErrClosed) || !errors.Is(directory.Close(), os.ErrClosed) {
		t.Fatal("owned Files not closed")
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, root.Name())) {
		t.Fatal("closed-directory failure changed original bytes/modes/entries")
	}
	for leaf, info := range map[string]os.FileInfo{".manifest-source": written, "manifest.json": targetInfo} {
		current, err := root.Lstat(leaf)
		if err != nil || !manifestWritten(current, info) || current.Mode() != info.Mode() {
			t.Fatal("closed-directory native failure changed identity:", err)
		}
	}
	current, err := root.Stat(".")
	if err != nil || !os.SameFile(current, borrowedInfo) {
		t.Fatal("native failure closed or changed borrowed Root:", err)
	}
}

func TestFileBoundaryManifestPublishBoundDirectory(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	roots, err := storage.OpenResourceRoots(state, storage.ResourceCode, "publisher", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	before := fileBoundaryTree(t, outside)
	bound, err := roots.Resource.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(roots.Path, roots.Path+".held"); err != nil {
		t.Fatal("move actual bound directory:", err)
	}
	manifestPublishDirectoryLink(t, roots.Path, outside)
	manifest := sampleManifest(t)
	if err := saveManifestForOptions(Options{StateRoot: state, SessionID: "publisher", fileRoot: roots.Resource}, manifest); err != nil {
		t.Fatal("bound production publisher:", err)
	}
	got, err := loadManifestFromRoot(roots.Resource, manifest.ID)
	if err != nil || !reflect.DeepEqual(got, manifest) {
		t.Fatal("bound publisher lost actual generation:", err)
	}
	current, err := roots.Resource.Stat(".")
	if err != nil || !os.SameFile(bound, current) {
		t.Fatal("publisher changed or closed borrowed root:", err)
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
		t.Fatal("publisher wrote/chmodded diagnostic junction outside")
	}
}

func TestFileBoundaryManifestPublishCreateWindowsExchangeDenied(t *testing.T) {
	opts := factoryDiskOptions(t)
	calls := 0
	session, err := createAgentSessionWithOpen(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, options jsonl.Options) (*jsonl.Store, error) {
		calls++
		opened, err := jsonl.OpenBound(id, roots, file, header, options)
		if err != nil {
			return opened, err
		}
		before, err := roots.Resource.Stat(".")
		if err != nil {
			t.Fatal(err)
		}
		renameErr := os.Rename(roots.Path, roots.Path+".held")
		if !errors.Is(renameErr, windows.ERROR_ACCESS_DENIED) {
			t.Fatal("long-lived journal must deny actual directory exchange with errno5:", renameErr)
		}
		after, err := roots.Resource.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			t.Fatal("denied directory exchange changed binding:", err)
		}
		return opened, nil
	})
	if session != nil {
		defer session.Close(t.Context())
	}
	if err != nil || session == nil || calls != 1 || opts.Model.(*testkit.FakeModel).Calls() != 0 {
		t.Fatal("default fixed publisher failed after denied exchange:", err)
	}
	got, err := loadManifestFromRoot(session.rt.opts.fileRoot, session.rt.generation)
	if err != nil || got.ID != session.rt.generation {
		t.Fatal("default Create failed actual bound publication:", err)
	}
}
