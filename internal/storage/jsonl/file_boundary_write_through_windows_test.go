//go:build windows

package jsonl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"golang.org/x/sys/windows"
)

// Step13 is an offline, test-only experiment. None of these helpers replaces
// Store, Repair, ReplaceFile, or Root.Rename. A passing test is not a durability
// certificate or approval to migrate the production replacement path.
const (
	step13WriteThroughSource  = "journal.jsonl.repairing"
	step13WriteThroughTarget  = "journal.jsonl"
	step13WriteThroughPayload = "step13 candidate payload\n"
	step13WriteThroughOldData = "step13 original journal\n"
	step13WriteThroughAccess  = windows.DELETE | windows.SYNCHRONIZE | windows.FILE_READ_ATTRIBUTES
	step13WriteThroughShare   = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE // Deliberately no DELETE sharing.
	step13WriteThroughOptions = windows.FILE_NON_DIRECTORY_FILE | windows.FILE_SYNCHRONOUS_IO_NONALERT |
		windows.FILE_OPEN_REPARSE_POINT | windows.FILE_OPEN_FOR_BACKUP_INTENT | windows.FILE_WRITE_THROUGH
	step13WriteThroughModeClass = 16 // Official FileModeInformation, not exported by x/sys v0.48.0.
)

// Public ntifs.h FILE_RENAME_INFORMATION ABI: BOOLEAN/ULONG union, HANDLE,
// ULONG byte length, WCHAR[1]. This is not Go's private fixed-MAX_PATH type.
// Class 10 reads the first BOOLEAN byte; the remaining union/padding is zero.
// https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_file_rename_information
// https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_file_mode_information
type step13WriteThroughRenameInformation struct {
	ReplaceOrFlags uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// Both directions must have nonnegative array lengths, so these are static
// offsetof assertions for the compiling architecture (HANDLE is pointer-sized).
type step13WriteThroughABI struct {
	rootA [int(unsafe.Offsetof(step13WriteThroughRenameInformation{}.RootDirectory)) - int(unsafe.Sizeof(uintptr(0)))]byte
	rootB [int(unsafe.Sizeof(uintptr(0))) - int(unsafe.Offsetof(step13WriteThroughRenameInformation{}.RootDirectory))]byte
	lenA  [int(unsafe.Offsetof(step13WriteThroughRenameInformation{}.FileNameLength)) - 2*int(unsafe.Sizeof(uintptr(0)))]byte
	lenB  [2*int(unsafe.Sizeof(uintptr(0))) - int(unsafe.Offsetof(step13WriteThroughRenameInformation{}.FileNameLength))]byte
	nameA [int(unsafe.Offsetof(step13WriteThroughRenameInformation{}.FileName)) - 2*int(unsafe.Sizeof(uintptr(0))) - 4]byte
	nameB [2*int(unsafe.Sizeof(uintptr(0))) + 4 - int(unsafe.Offsetof(step13WriteThroughRenameInformation{}.FileName))]byte
}

type step13WriteThroughCalls struct {
	opens, queries, renames, closes, removes int
	mode                                     uint32
	openStatus, queryStatus                  error
	renameStatus                             error
}

func step13WriteThroughErrno(err error) error {
	var status windows.NTStatus
	if errors.As(err, &status) {
		return status.Errno()
	}
	return err
}

func step13WriteThroughInvalid() error {
	return product.NewError(product.CodeInvalidArgument, "step13 source or directory identity is not ordinary and bound")
}

// root/directory are borrowed and must stay valid without concurrent Close.
// NewFile owns only the returned source handle; rejection always closes it.
func step13WriteThroughOpen(root *os.Root, directory *os.File, beforeOpen func(), calls *step13WriteThroughCalls) (*os.File, error) {
	bound, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	openedDir, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	if !bound.IsDir() || store.IsReparseInfo(bound) || !openedDir.IsDir() || store.IsReparseInfo(openedDir) || !os.SameFile(bound, openedDir) {
		return nil, step13WriteThroughInvalid()
	}
	before, err := root.Lstat(step13WriteThroughSource)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || store.IsReparseInfo(before) {
		return nil, step13WriteThroughInvalid()
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	name, err := windows.NewNTUnicodeString(step13WriteThroughSource)
	if err != nil {
		return nil, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(directory.Fd()), ObjectName: name,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var handle windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	calls.opens++
	err = windows.NtCreateFile(&handle, step13WriteThroughAccess, &attrs, &iosb, nil, 0,
		step13WriteThroughShare, windows.FILE_OPEN, step13WriteThroughOptions, 0, 0)
	calls.openStatus = err
	runtime.KeepAlive(name)
	runtime.KeepAlive(directory)
	if err != nil {
		return nil, step13WriteThroughErrno(err)
	}
	file := os.NewFile(uintptr(handle), "step13-relative-source")
	if file == nil {
		calls.closes++
		return nil, errors.Join(errors.New("step13 wrap native source handle"), windows.CloseHandle(handle))
	}
	fail := func(err error) (*os.File, error) {
		return nil, errors.Join(err, step13WriteThroughClose(file, calls))
	}
	opened, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	if !opened.Mode().IsRegular() || store.IsReparseInfo(opened) || !os.SameFile(before, opened) {
		return fail(step13WriteThroughInvalid())
	}
	current, err := root.Lstat(step13WriteThroughSource)
	if err != nil {
		return fail(err)
	}
	if !current.Mode().IsRegular() || store.IsReparseInfo(current) || !os.SameFile(opened, current) {
		return fail(step13WriteThroughInvalid())
	}
	if err := step13WriteThroughMode(file, calls); err != nil {
		return fail(err)
	}
	return file, nil
}

func step13WriteThroughMode(file *os.File, calls *step13WriteThroughCalls) error {
	calls.mode = 0  // A failed query cannot retain an earlier mode-bit proof.
	var mode uint32 // Official FILE_MODE_INFORMATION contains one ULONG.
	var iosb windows.IO_STATUS_BLOCK
	calls.queries++
	err := windows.NtQueryInformationFile(windows.Handle(file.Fd()), &iosb, (*byte)(unsafe.Pointer(&mode)), uint32(unsafe.Sizeof(mode)), step13WriteThroughModeClass)
	calls.queryStatus = err
	runtime.KeepAlive(file)
	if err != nil {
		return step13WriteThroughErrno(err)
	}
	calls.mode = mode
	if iosb.Information != unsafe.Sizeof(mode) || mode&windows.FILE_WRITE_THROUGH == 0 || mode&windows.FILE_SYNCHRONOUS_IO_NONALERT == 0 {
		return fmt.Errorf("BLOCKED: same-source FileModeInformation bytes=%d mode=%#x lacks required write-through/synchronous bits", iosb.Information, mode)
	}
	return nil
}

func step13WriteThroughBuffer(directory *os.File) ([]uintptr, uint32, error) {
	name, err := windows.UTF16FromString(step13WriteThroughTarget)
	if err != nil {
		return nil, 0, err
	}
	name = name[:len(name)-1] // FileNameLength excludes the NUL and is in bytes.
	header := unsafe.Sizeof(step13WriteThroughRenameInformation{})
	nameOffset := unsafe.Offsetof(step13WriteThroughRenameInformation{}.FileName)
	word := unsafe.Sizeof(uintptr(0))
	// The public document requires >= sizeof(struct) + FileName byte size.
	// Fixed leaf and checked arithmetic keep both ULONG length and allocation safe.
	if uint64(len(name)) > (uint64(^uint32(0))-uint64(header))/2 {
		return nil, 0, errors.New("step13 rename buffer exceeds ULONG length")
	}
	size := header + uintptr(len(name))*2
	if size > uintptr(int(^uint(0)>>1))-word+1 || nameOffset+uintptr(len(name))*2 > size {
		return nil, 0, errors.New("step13 rename buffer exceeds allocation length")
	}
	buffer := make([]uintptr, (size+word-1)/word) // Explicit HANDLE-aligned storage.
	base := unsafe.Pointer(&buffer[0])
	info := (*step13WriteThroughRenameInformation)(base)
	info.ReplaceOrFlags = 1 // ReplaceIfExists=TRUE, traditional class only.
	info.RootDirectory = windows.Handle(directory.Fd())
	info.FileNameLength = uint32(len(name) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Add(base, nameOffset)), len(name)), name)
	return buffer, uint32(size), nil
}

func step13WriteThroughRename(file, directory *os.File, calls *step13WriteThroughCalls) error {
	if err := step13WriteThroughMode(file, calls); err != nil {
		return err // Query failure or absent mode bit blocks the actual rename.
	}
	buffer, length, err := step13WriteThroughBuffer(directory)
	if err != nil {
		return err
	}
	var iosb windows.IO_STATUS_BLOCK
	calls.renames++
	err = windows.NtSetInformationFile(windows.Handle(file.Fd()), &iosb, (*byte)(unsafe.Pointer(&buffer[0])), length, windows.FileRenameInformation)
	calls.renameStatus = err
	runtime.KeepAlive(buffer)
	runtime.KeepAlive(directory)
	runtime.KeepAlive(file)
	return step13WriteThroughErrno(err)
}

func step13WriteThroughClose(file *os.File, calls *step13WriteThroughCalls) error {
	calls.closes++
	return file.Close()
}

func step13WriteThroughFixture(t *testing.T) (*os.Root, *os.File, string) {
	t.Helper()
	if runtime.GOARCH != "amd64" {
		t.Skip("Step13 write-through runtime experiment is authorized for Windows/amd64 only; other architectures unverified")
	}
	state := t.TempDir() // Test-owned trusted anchor, stable outside ancestors.
	roots, err := store.OpenResourceRoots(state, store.ResourceCode, "step13", true, nil)
	if err != nil {
		t.Fatal("checked resource root fixture:", err)
	}
	t.Cleanup(func() { _ = roots.Close() })
	directory, err := roots.Resource.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	return roots.Resource, directory, filepath.Join(state, "sessions", "step13")
}

func step13WriteThroughWrite(t *testing.T, root *os.Root, leaf, data string) os.FileInfo {
	t.Helper()
	file, err := root.OpenFile(leaf, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	n, writeErr := file.Write([]byte(data))
	syncErr := file.Sync() // Candidate payload preparation, not rename-mode proof.
	info, statErr := file.Stat()
	closeErr := file.Close()
	if n != len(data) || writeErr != nil || syncErr != nil || statErr != nil || closeErr != nil {
		t.Fatalf("fixture write n=%d write=%v sync=%v stat=%v close=%v", n, writeErr, syncErr, statErr, closeErr)
	}
	return info // Stat by handle, with file ID already loaded; never a path reopen.
}

func TestFileBoundaryWriteThroughGateModeAndABI(t *testing.T) {
	root, directory, path := step13WriteThroughFixture(t)
	var _ step13WriteThroughABI
	if unsafe.Sizeof(step13WriteThroughRenameInformation{}) != 24 || unsafe.Offsetof(step13WriteThroughRenameInformation{}.RootDirectory) != 8 ||
		unsafe.Offsetof(step13WriteThroughRenameInformation{}.FileNameLength) != 16 || unsafe.Offsetof(step13WriteThroughRenameInformation{}.FileName) != 20 {
		t.Fatal("BLOCKED: public amd64 FILE_RENAME_INFORMATION ABI mismatch")
	}
	before := step13WriteThroughWrite(t, root, step13WriteThroughSource, step13WriteThroughPayload)
	calls := step13WriteThroughCalls{}
	file, err := step13WriteThroughOpen(root, directory, nil, &calls)
	if err != nil {
		t.Fatalf("BLOCKED: required native access/share/mode: calls=%+v error=%v", calls, err)
	}
	t.Cleanup(func() { _ = file.Close() })
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		t.Fatal("native source file object is not the prepared candidate", err)
	}
	if n, err := file.Write([]byte("forbidden")); n != 0 || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("minimal rename handle unexpectedly has WRITE_DATA: n=%d err=%v", n, err)
	}
	// Real other-process rename/delete attempts must not swap this source object.
	step12NativeProbe(t, filepath.Join(path, step13WriteThroughSource), "leaf-blocked")
	if err := step13WriteThroughRename(file, directory, &calls); err != nil {
		t.Fatalf("BLOCKED: traditional rename on same no-SHARE_DELETE write-through handle: calls=%+v error=%v", calls, err)
	}
	if err := step13WriteThroughMode(file, &calls); err != nil {
		t.Fatal("post-rename same-object mode:", err)
	}
	after, err := root.Lstat(step13WriteThroughTarget)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("published leaf is not the original prepared source identity", err)
	}
	if err := step13WriteThroughClose(file, &calls); err != nil {
		t.Fatal(err)
	}
	data, err := root.ReadFile(step13WriteThroughTarget)
	if err != nil || string(data) != step13WriteThroughPayload {
		t.Fatal("published bytes:", string(data), err)
	}
	if _, err := root.Lstat(step13WriteThroughSource); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful rename retained source name:", err)
	}
	if calls.opens != 1 || calls.queries != 3 || calls.renames != 1 || calls.closes != 1 {
		t.Fatalf("actual native call counts=%+v", calls)
	}
	t.Logf("ABI sizeof=24 offsets=0/8/16/20; access=%#x share=%#x objectattrs=%#x options=%#x; same-object mode=%#x; calls=%+v; WRITE_DATA denied; source protected across processes; traditional rename succeeded", step13WriteThroughAccess, step13WriteThroughShare, windows.OBJ_CASE_INSENSITIVE|windows.OBJ_DONT_REPARSE, step13WriteThroughOptions, calls.mode, calls)
}

// Snapshot every entry through the bound Root. Attributes is the entire Windows
// attribute bitmask, not merely the READONLY/reparse subset. Access times are not
// compared because the experiment's own reads can change them.
type step13WriteThroughEntry struct {
	Mode       os.FileMode
	Attributes uint32
	Size       int64
	Created    int64
	Written    int64
	Data       string
}

func step13WriteThroughSnapshot(t *testing.T, root *os.Root) map[string]step13WriteThroughEntry {
	t.Helper()
	out := make(map[string]step13WriteThroughEntry)
	var walk func(string)
	walk = func(leaf string) {
		info, err := root.Lstat(leaf)
		if err != nil {
			t.Fatal("snapshot stat:", err)
		}
		attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
		if !ok || attrs == nil {
			t.Fatal("snapshot lacks full Windows attributes")
		}
		entry := step13WriteThroughEntry{Mode: info.Mode(), Attributes: attrs.FileAttributes, Size: info.Size(), Created: attrs.CreationTime.Nanoseconds(), Written: attrs.LastWriteTime.Nanoseconds()}
		if info.Mode().IsRegular() && !store.IsReparseInfo(info) {
			data, err := root.ReadFile(leaf)
			if err != nil {
				t.Fatal("snapshot read:", err)
			}
			entry.Data = string(data)
		}
		out[leaf] = entry
		if !info.IsDir() || store.IsReparseInfo(info) {
			return
		}
		file, err := root.Open(leaf)
		if err != nil {
			t.Fatal("snapshot directory:", err)
		}
		children, readErr := file.ReadDir(-1)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			t.Fatal(errors.Join(readErr, closeErr))
		}
		for _, child := range children {
			walk(filepath.Join(leaf, child.Name()))
		}
	}
	walk(".")
	return out
}

func step13WriteThroughCode(t *testing.T, err error) syscall.Errno {
	t.Helper()
	if err == nil {
		return 0
	}
	var code syscall.Errno
	if !errors.As(step13WriteThroughErrno(err), &code) {
		t.Fatalf("actual API error is not a numeric Win32 errno: %v", err)
	}
	return code
}

func step13WriteThroughStatus(err error) string {
	if err == nil {
		return "SUCCESS"
	}
	var status windows.NTStatus
	if errors.As(err, &status) {
		return fmt.Sprintf("NTSTATUS=%#x errno=%d", uint32(status), status.Errno())
	}
	var code syscall.Errno
	if errors.As(err, &code) {
		return fmt.Sprintf("errno=%d", code)
	}
	return fmt.Sprintf("non-API error=%v", err)
}

// Win32 path opens below are independent cold-fixture holders, not candidate
// I/O. Source/target/parent are never dynamically redirected in the baseline.
func step13WriteThroughHolder(t *testing.T, path string, access, share uint32) *os.File {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, access, share, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal("sharing fixture open:", err)
	}
	file := os.NewFile(uintptr(handle), "step13-fixture-holder")
	if file == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("wrap fixture holder")
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

type step13WriteThroughObservation struct {
	valid, published bool
	code             syscall.Errno
	calls            step13WriteThroughCalls
	legacyCalls      int
}

func TestFileBoundaryWriteThroughGateCompare(t *testing.T) {
	// These are characterization tests, not an assertion that the APIs are
	// equivalent. Every differing result is explicitly logged as BLOCKED.
	for _, kind := range []string{
		"target-missing", "target-ordinary", "target-shared-delete", "target-root-reader",
		"target-no-delete-share", "target-legacy-reader", "target-write-holder", "target-readonly",
		"target-directory", "source-readonly", "source-legacy-reader", "source-existing-delete",
		"case-insensitive-leaves",
	} {
		t.Run(kind, func(t *testing.T) {
			observations := make(map[string]step13WriteThroughObservation)
			for _, method := range []string{"legacy", "native"} {
				t.Run(method, func(t *testing.T) {
					root, directory, path := step13WriteThroughFixture(t)
					sourceLeaf, targetLeaf := step13WriteThroughSource, step13WriteThroughTarget
					if kind == "case-insensitive-leaves" {
						sourceLeaf, targetLeaf = strings.ToUpper(sourceLeaf), strings.ToUpper(targetLeaf)
					}
					sourceInfo := step13WriteThroughWrite(t, root, sourceLeaf, step13WriteThroughPayload)
					var targetInfo os.FileInfo
					if kind == "target-directory" {
						if err := root.Mkdir(targetLeaf, 0700); err != nil {
							t.Fatal(err)
						}
						step13WriteThroughWrite(t, root, filepath.Join(targetLeaf, "child"), "directory child sentinel")
						targetInfo, _ = root.Lstat(targetLeaf)
					} else if kind != "target-missing" {
						targetInfo = step13WriteThroughWrite(t, root, targetLeaf, step13WriteThroughOldData)
					}
					if kind == "target-readonly" || kind == "source-readonly" {
						leaf := targetLeaf
						if kind == "source-readonly" {
							leaf = sourceLeaf
						}
						if err := root.Chmod(leaf, 0400); err != nil {
							t.Fatal("readonly fixture:", err)
						}
						t.Cleanup(func() { _ = root.Chmod(sourceLeaf, 0600); _ = root.Chmod(targetLeaf, 0600) })
					}
					// Root.Lstat and File.Stat return handle-observed, already loaded
					// IDs on this Go version; case aliases must be the same object.
					alias, err := root.Lstat(step13WriteThroughSource)
					if err != nil || !os.SameFile(sourceInfo, alias) {
						t.Fatal("source lower/nativeCase identity:", err)
					}
					before := step13WriteThroughSnapshot(t, root)
					var held *os.File
					heldTarget := false
					switch kind {
					case "target-shared-delete":
						held = step13WriteThroughHolder(t, filepath.Join(path, targetLeaf), windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
						heldTarget = true
					case "target-root-reader":
						held, err = root.Open(targetLeaf) // Go Root reader shares READ|WRITE|DELETE.
						heldTarget = true
					case "target-no-delete-share":
						held = step13WriteThroughHolder(t, filepath.Join(path, targetLeaf), windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
						heldTarget = true
					case "target-legacy-reader", "source-legacy-reader":
						leaf := targetLeaf
						if kind == "source-legacy-reader" {
							leaf = sourceLeaf
						} else {
							heldTarget = true
						}
						held, err = os.OpenFile(filepath.Join(path, leaf), os.O_RDONLY, 0) // Old reader shares READ|WRITE, no DELETE.
					case "target-write-holder":
						held = step13WriteThroughHolder(t, filepath.Join(path, targetLeaf), windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE)
						heldTarget = true
					case "source-existing-delete":
						held = step13WriteThroughHolder(t, filepath.Join(path, sourceLeaf), step13WriteThroughAccess, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
					}
					if err != nil {
						t.Fatal("reader fixture:", err)
					}
					if held != nil {
						t.Cleanup(func() { _ = held.Close() })
					}
					got := step13WriteThroughObservation{}
					var file *os.File
					if method == "legacy" {
						got.legacyCalls++
						err = store.ReplaceFile(filepath.Join(path, step13WriteThroughSource), filepath.Join(path, step13WriteThroughTarget))
					} else {
						file, err = step13WriteThroughOpen(root, directory, nil, &got.calls)
						if err != nil && kind != "source-legacy-reader" && kind != "source-existing-delete" {
							t.Fatalf("BLOCKED: prerequisite access/share/mode rejected at open, no rename: calls=%+v err=%v", got.calls, err)
						}
						if file != nil {
							t.Cleanup(func() { _ = file.Close() })
							err = step13WriteThroughRename(file, directory, &got.calls)
							if closeErr := step13WriteThroughClose(file, &got.calls); closeErr != nil {
								t.Fatal("source close:", closeErr)
							}
						}
					}
					got.code = step13WriteThroughCode(t, err)
					got.published = err == nil
					if held != nil {
						if heldTarget {
							info, statErr := held.Stat()
							if statErr != nil || !os.SameFile(targetInfo, info) {
								t.Fatal("held target identity changed:", statErr)
							}
							if kind != "target-write-holder" {
								data := make([]byte, len(step13WriteThroughOldData))
								n, readErr := held.ReadAt(data, 0)
								if readErr != nil || n != len(data) || string(data) != step13WriteThroughOldData {
									t.Fatal("held old target bytes changed:", readErr)
								}
							}
						}
						if err := held.Close(); err != nil {
							t.Fatal(err)
						}
					}
					after := step13WriteThroughSnapshot(t, root)
					if got.published {
						if _, statErr := root.Lstat(step13WriteThroughSource); !errors.Is(statErr, os.ErrNotExist) {
							t.Fatal("success retained source leaf:", statErr)
						}
						info, statErr := root.Lstat(step13WriteThroughTarget)
						if statErr != nil || !os.SameFile(sourceInfo, info) || after[step13WriteThroughTarget].Data != step13WriteThroughPayload {
							t.Fatal("success did not publish source bytes/identity:", statErr)
						}
						if after[step13WriteThroughTarget].Attributes != before[sourceLeaf].Attributes || after[step13WriteThroughTarget].Mode != before[sourceLeaf].Mode {
							t.Fatal("success changed source attribute bitmask/mode")
						}
						if len(after) != 2 {
							t.Fatalf("success retained unexpected entries: %+v", after)
						}
					} else {
						if !reflect.DeepEqual(before, after) {
							t.Fatalf("BLOCKED: actual API failure partially changed bytes/modes/attributes/entries/timestamps: before=%+v after=%+v", before, after)
						}
						info, statErr := root.Lstat(step13WriteThroughSource)
						if statErr != nil || !os.SameFile(sourceInfo, info) {
							t.Fatal("failed operation changed source identity:", statErr)
						}
						if targetInfo != nil {
							info, statErr = root.Lstat(step13WriteThroughTarget)
							if statErr != nil || !os.SameFile(targetInfo, info) {
								t.Fatal("failed operation changed target identity:", statErr)
							}
						}
					}
					if got.legacyCalls != 1 && method == "legacy" || method == "native" && (got.calls.opens != 1 || got.calls.renames > 1) {
						t.Fatalf("unexpected actual call counts: %+v", got)
					}
					if method == "native" && file != nil && (got.calls.queries != 2 || got.calls.renames != 1 || got.calls.closes != 1 || got.calls.mode&windows.FILE_WRITE_THROUGH == 0) {
						t.Fatalf("same-file-object mode/call-count evidence missing: %+v", got.calls)
					}
					if method == "native" && file == nil && (got.calls.queries != 0 || got.calls.renames != 0 || got.code != windows.ERROR_SHARING_VIOLATION) {
						t.Fatalf("expected source sharing refusal got calls=%+v errno=%d", got.calls, got.code)
					}
					got.valid = true
					observations[method] = got
					var entries []string
					for leaf := range after {
						entries = append(entries, leaf)
					}
					t.Logf("%s oldFlags=0x9 errno=%d published=%v legacyCalls=%d nativeCalls(open/query/rename/close)=%d/%d/%d/%d mode=%#x raw=%s/%s/%s; sourceAttrs=%#x targetAttrs=%#x targetMode=%v targetSize=%d entries=%v; identity/bytes/full attrs and failed-call no-partial-change asserted", method, got.code, got.published, got.legacyCalls, got.calls.opens, got.calls.queries, got.calls.renames, got.calls.closes, got.calls.mode, step13WriteThroughStatus(got.calls.openStatus), step13WriteThroughStatus(got.calls.queryStatus), step13WriteThroughStatus(got.calls.renameStatus), before[sourceLeaf].Attributes, after[step13WriteThroughTarget].Attributes, after[step13WriteThroughTarget].Mode, after[step13WriteThroughTarget].Size, entries)
				})
			}
			old, native := observations["legacy"], observations["native"]
			if old.valid && native.valid {
				equal := old.code == native.code && old.published == native.published
				t.Logf("old-vs-native result/error equivalent=%v old(errno=%d,published=%v) native(errno=%d,published=%v)", equal, old.code, old.published, native.code, native.published)
				if !equal {
					t.Log("BLOCKED: complete old replacement semantics are not equivalent for this input; experiment records the difference without approving a weaker contract")
				}
			}
		})
	}
}

func TestFileBoundaryWriteThroughGateBoundDirectory(t *testing.T) {
	root, directory, path := step13WriteThroughFixture(t)
	outside := t.TempDir()
	for _, leaf := range []string{step13WriteThroughSource, step13WriteThroughTarget, "sentinel"} {
		if err := os.WriteFile(filepath.Join(outside, leaf), []byte("outside:"+leaf), 0644); err != nil {
			t.Fatal(err)
		}
	}
	attrPath, err := windows.UTF16PtrFromString(filepath.Join(outside, "sentinel"))
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetFileAttributes(attrPath, windows.FILE_ATTRIBUTE_READONLY|windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_ATTRIBUTE_ARCHIVE); err != nil {
		t.Fatal("outside full-attribute fixture:", err)
	}
	external, err := os.OpenRoot(outside)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = external.Close() })
	before := step13WriteThroughSnapshot(t, external)
	if err := os.Rename(path, path+".held"); err != nil { // Adversarial fixture operation only.
		t.Fatal("retained Root/directory cannot be moved before candidate creation:", err)
	}
	fileBoundaryDirectoryLink(t, path, outside)
	// All candidate preparation, native open, rename, observation and cleanup
	// happen only after the stale original name became an outside junction.
	original := step13WriteThroughWrite(t, root, step13WriteThroughSource, step13WriteThroughPayload)
	step13WriteThroughWrite(t, root, step13WriteThroughTarget, step13WriteThroughOldData)
	calls := step13WriteThroughCalls{}
	file, err := step13WriteThroughOpen(root, directory, nil, &calls)
	if err != nil {
		t.Fatalf("BLOCKED: post-move same-directory native source: %+v %v", calls, err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if err := step13WriteThroughRename(file, directory, &calls); err != nil {
		t.Fatal("post-move relative traditional rename:", err)
	}
	published, err := root.Lstat(step13WriteThroughTarget)
	if err != nil || !os.SameFile(original, published) {
		t.Fatal("post-move publication lost original object:", err)
	}
	if err := step13WriteThroughClose(file, &calls); err != nil {
		t.Fatal(err)
	}
	data, err := root.ReadFile(step13WriteThroughTarget)
	if err != nil || string(data) != step13WriteThroughPayload {
		t.Fatal("original bound directory payload:", err)
	}
	if _, err := root.Lstat(step13WriteThroughSource); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source retained after relative rename:", err)
	}
	if err := root.Remove(step13WriteThroughTarget); err != nil {
		t.Fatal("bound cleanup:", err)
	}
	calls.removes++
	if !reflect.DeepEqual(before, step13WriteThroughSnapshot(t, external)) {
		t.Error("outside bytes/modes/full Windows attributes/entries/timestamps changed")
	}
	entries, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	names, readErr := entries.Readdirnames(-1)
	closeErr := entries.Close()
	if readErr != nil || closeErr != nil || len(names) != 0 || calls.opens != 1 || calls.renames != 1 || calls.closes != 1 || calls.removes != 1 {
		t.Fatalf("relative publication/cleanup entries=%v calls=%+v read=%v close=%v", names, calls, readErr, closeErr)
	}
	t.Logf("original directory renamed and stale name became outside junction before candidate create/write/Sync/close; source/target RootDirectory same live bound directory; original payload asserted then bound cleanup; outside full snapshot unchanged; calls=%+v", calls)
}

func TestFileBoundaryWriteThroughGateRejectsSource(t *testing.T) {
	for _, kind := range []string{"directory", "junction", "file-symlink", "ordinary-A-to-B", "foreign-directory"} {
		t.Run(kind, func(t *testing.T) {
			root, directory, path := step13WriteThroughFixture(t)
			outside := t.TempDir()
			externalPath := filepath.Join(outside, "outside-source")
			if err := os.WriteFile(externalPath, []byte("outside-source sentinel"), 0644); err != nil {
				t.Fatal(err)
			}
			external, err := os.OpenRoot(outside)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = external.Close() })
			var beforeOpen func()
			sourcePath := filepath.Join(path, step13WriteThroughSource)
			switch kind {
			case "directory":
				if err := root.Mkdir(step13WriteThroughSource, 0700); err != nil {
					t.Fatal(err)
				}
			case "junction":
				fileBoundaryDirectoryLink(t, sourcePath, outside)
			case "file-symlink":
				fileBoundaryFileLink(t, sourcePath, externalPath)
			case "ordinary-A-to-B":
				step13WriteThroughWrite(t, root, step13WriteThroughSource, step13WriteThroughPayload)
				beforeOpen = func() {
					if err := os.Rename(sourcePath, sourcePath+".held"); err != nil {
						t.Fatal(err)
					}
					if err := os.Link(externalPath, sourcePath); err != nil {
						t.Fatal(err)
					}
				}
			case "foreign-directory":
				step13WriteThroughWrite(t, root, step13WriteThroughSource, step13WriteThroughPayload)
				foreign, err := external.Open(".")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = foreign.Close() })
				directory = foreign // root and directory identity mismatch, no native call.
			}
			before := step13WriteThroughSnapshot(t, external)
			calls := step13WriteThroughCalls{}
			file, err := step13WriteThroughOpen(root, directory, beforeOpen, &calls)
			if file != nil {
				_ = step13WriteThroughClose(file, &calls)
				t.Fatal("rejected source returned owned handle")
			}
			var pe *product.Error
			if !errors.As(err, &pe) || pe.Code != product.CodeInvalidArgument {
				t.Fatalf("source rejection=%v, want invalid_argument", err)
			}
			wantOpens := 0
			if kind == "ordinary-A-to-B" {
				wantOpens = 1
				if calls.closes != 1 {
					t.Fatal("opened replacement source was not closed:", calls)
				}
				// This rename can succeed only after the rejected native no-DELETE
				// handle was actually released. It is an adversarial fixture probe.
				if err := os.Rename(sourcePath, sourcePath+".rejected"); err != nil {
					t.Fatal("rejection leaked source handle:", err)
				}
				data, err := root.ReadFile(step13WriteThroughSource + ".held")
				if err != nil || string(data) != step13WriteThroughPayload {
					t.Fatal("original A changed:", err)
				}
			}
			if calls.opens != wantOpens || calls.queries != 0 || calls.renames != 0 {
				t.Fatalf("rejection actual counts=%+v", calls)
			}
			if _, err := root.Lstat(step13WriteThroughTarget); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejection published target:", err)
			}
			if !reflect.DeepEqual(before, step13WriteThroughSnapshot(t, external)) {
				t.Error("source rejection changed outside bytes/modes/full attrs/entries")
			}
			t.Logf("%s rejected invalid_argument; native calls=%+v; no publication; outside full snapshot unchanged", kind, calls)
		})
	}
}

// Cleanup has a per-call barrier solely to test real Close/remove/identity
// failures. It closes the owned source before attempting a name-based remove,
// preserves every error, and refuses to remove an observed foreign object.
// The SameFile check is not an atomic expected-FileID delete condition.
func step13WriteThroughCleanup(root *os.Root, file *os.File, expected os.FileInfo, afterClose func(), primary error, calls *step13WriteThroughCalls) error {
	closeErr := step13WriteThroughClose(file, calls)
	if closeErr != nil {
		return errors.Join(primary, closeErr)
	}
	if afterClose != nil {
		afterClose()
	}
	current, err := root.Lstat(step13WriteThroughSource)
	if err != nil {
		return errors.Join(primary, err)
	}
	if !current.Mode().IsRegular() || store.IsReparseInfo(current) || !os.SameFile(expected, current) {
		return errors.Join(primary, step13WriteThroughInvalid())
	}
	calls.removes++
	return errors.Join(primary, root.Remove(step13WriteThroughSource))
}

func TestFileBoundaryWriteThroughGateFailures(t *testing.T) {
	for _, kind := range []string{"short-rename-buffer", "closed-target-directory", "cleanup-sharing-refusal", "cleanup-close-failure", "cleanup-foreign-leaf", "post-publication-close-failure"} {
		t.Run(kind, func(t *testing.T) {
			root, directory, path := step13WriteThroughFixture(t)
			sourceInfo := step13WriteThroughWrite(t, root, step13WriteThroughSource, step13WriteThroughPayload)
			step13WriteThroughWrite(t, root, step13WriteThroughTarget, step13WriteThroughOldData)
			before := step13WriteThroughSnapshot(t, root)
			calls := step13WriteThroughCalls{}
			file, err := step13WriteThroughOpen(root, directory, nil, &calls)
			if err != nil {
				t.Fatalf("BLOCKED: failure-case prerequisite %+v %v", calls, err)
			}
			t.Cleanup(func() { _ = file.Close() })
			if kind == "post-publication-close-failure" {
				if err := step13WriteThroughRename(file, directory, &calls); err != nil {
					t.Fatal(err)
				}
				if err := step13WriteThroughClose(file, &calls); err != nil {
					t.Fatal(err)
				}
				err := step13WriteThroughClose(file, &calls) // Real second Close, not an injected error value.
				if !errors.Is(err, os.ErrClosed) {
					t.Fatal("second source close:", err)
				}
				targetInfo, err := root.Lstat(step13WriteThroughTarget)
				if err != nil || !os.SameFile(sourceInfo, targetInfo) {
					t.Fatal("close error changed published identity:", err)
				}
				data, err := root.ReadFile(step13WriteThroughTarget)
				if err != nil || string(data) != step13WriteThroughPayload {
					t.Fatal("close error lost published data:", err)
				}
				if _, err := root.Lstat(step13WriteThroughSource); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("publication retained source:", err)
				}
				t.Logf("actual rename succeeded once; second Close=ErrClosed; publication remains changed and is not rolled back; calls=%+v", calls)
				return
			}
			if kind == "closed-target-directory" {
				if err := directory.Close(); err != nil {
					t.Fatal(err)
				}
				err = step13WriteThroughRename(file, directory, &calls)
			} else {
				if err := step13WriteThroughMode(file, &calls); err != nil {
					t.Fatal(err)
				}
				buffer, _, bufferErr := step13WriteThroughBuffer(directory)
				if bufferErr != nil {
					t.Fatal(bufferErr)
				}
				var iosb windows.IO_STATUS_BLOCK
				calls.renames++
				// Deliberately shorter than the public ABI header: real NT failure.
				err = windows.NtSetInformationFile(windows.Handle(file.Fd()), &iosb, (*byte)(unsafe.Pointer(&buffer[0])), uint32(unsafe.Offsetof(step13WriteThroughRenameInformation{}.FileNameLength)), windows.FileRenameInformation)
				calls.renameStatus = err
				runtime.KeepAlive(buffer)
				runtime.KeepAlive(file)
				runtime.KeepAlive(directory)
			}
			primary := step13WriteThroughErrno(err)
			code := step13WriteThroughCode(t, primary)
			wantCode, wantStatus := syscall.Errno(windows.ERROR_BAD_LENGTH), windows.STATUS_INFO_LENGTH_MISMATCH
			if kind == "closed-target-directory" {
				wantCode, wantStatus = windows.ERROR_INVALID_HANDLE, windows.STATUS_OBJECT_TYPE_MISMATCH
			}
			if primary == nil || code != wantCode || !errors.Is(calls.renameStatus, wantStatus) || calls.renames != 1 || calls.mode&windows.FILE_WRITE_THROUGH == 0 {
				t.Fatalf("invalid input did not produce one real same-mode rename failure with expected numeric code: calls=%+v err=%v want errno=%d NTSTATUS=%#x", calls, primary, wantCode, uint32(wantStatus))
			}
			if !reflect.DeepEqual(before, step13WriteThroughSnapshot(t, root)) {
				t.Fatal("real rename failure partially changed fixture before cleanup")
			}
			var blocker *os.File
			var afterClose func()
			var external *os.Root
			var outsideBefore map[string]step13WriteThroughEntry
			switch kind {
			case "cleanup-sharing-refusal":
				afterClose = func() {
					var err error
					blocker, err = os.OpenFile(filepath.Join(path, step13WriteThroughSource), os.O_RDONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = blocker.Close() })
				}
			case "cleanup-close-failure":
				if err := step13WriteThroughClose(file, &calls); err != nil {
					t.Fatal(err)
				}
			case "cleanup-foreign-leaf":
				outside := t.TempDir()
				externalPath := filepath.Join(outside, "foreign")
				if err := os.WriteFile(externalPath, []byte("foreign cleanup sentinel"), 0644); err != nil {
					t.Fatal(err)
				}
				var err error
				external, err = os.OpenRoot(outside)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = external.Close() })
				outsideBefore = step13WriteThroughSnapshot(t, external)
				afterClose = func() {
					sourcePath := filepath.Join(path, step13WriteThroughSource)
					if err := os.Rename(sourcePath, sourcePath+".held"); err != nil {
						t.Fatal(err)
					}
					if err := os.Link(externalPath, sourcePath); err != nil {
						t.Fatal(err)
					}
				}
			}
			cleanupErr := step13WriteThroughCleanup(root, file, sourceInfo, afterClose, primary, &calls)
			if !errors.Is(cleanupErr, code) {
				t.Fatalf("cleanup discarded real rename errno=%d: %v", code, cleanupErr)
			}
			switch kind {
			case "cleanup-sharing-refusal":
				if !errors.Is(cleanupErr, windows.ERROR_SHARING_VIOLATION) || calls.removes != 1 {
					t.Fatalf("actual cleanup sharing refusal lost: calls=%+v err=%v", calls, cleanupErr)
				}
				if err := blocker.Close(); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, step13WriteThroughSnapshot(t, root)) {
					t.Fatal("failed cleanup changed retained source/target")
				}
				if err := root.Remove(step13WriteThroughSource); err != nil {
					t.Fatal("released cleanup fixture still blocked:", err)
				}
			case "cleanup-close-failure":
				if !errors.Is(cleanupErr, os.ErrClosed) || calls.removes != 0 || !reflect.DeepEqual(before, step13WriteThroughSnapshot(t, root)) {
					t.Fatalf("actual Close failure not preserved with no remove: %+v %v", calls, cleanupErr)
				}
			case "cleanup-foreign-leaf":
				var pe *product.Error
				if !errors.As(cleanupErr, &pe) || pe.Code != product.CodeInvalidArgument || calls.removes != 0 {
					t.Fatalf("foreign cleanup leaf not rejected: calls=%+v err=%v", calls, cleanupErr)
				}
				if !reflect.DeepEqual(outsideBefore, step13WriteThroughSnapshot(t, external)) {
					t.Fatal("cleanup altered foreign object's outside snapshot")
				}
				if _, err := root.Lstat(step13WriteThroughSource); err != nil {
					t.Fatal("cleanup deleted foreign source alias:", err)
				}
			default:
				if calls.removes != 1 {
					t.Fatal("failure cleanup was not invoked:", calls)
				}
				if _, err := root.Lstat(step13WriteThroughSource); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("owned failed source was not cleaned:", err)
				}
			}
			data, err := root.ReadFile(step13WriteThroughTarget)
			if err != nil || string(data) != step13WriteThroughOldData {
				t.Fatal("failed rename/cleanup changed old journal:", err)
			}
			if _, err := file.Stat(); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
				t.Fatal("failure path leaked native source:", err)
			}
			t.Logf("%s raw rename=%s converted errno=%d; pre-cleanup partialChange=false; cleanup errors retained=%v; source closed; target old bytes; actual calls=%+v", kind, step13WriteThroughStatus(calls.renameStatus), code, cleanupErr, calls)
		})
	}
}

func TestFileBoundaryWriteThroughGateModeGuard(t *testing.T) {
	for _, kind := range []string{"ordinary-handle-without-write-through", "closed-native-source"} {
		t.Run(kind, func(t *testing.T) {
			root, directory, _ := step13WriteThroughFixture(t)
			step13WriteThroughWrite(t, root, step13WriteThroughSource, step13WriteThroughPayload)
			step13WriteThroughWrite(t, root, step13WriteThroughTarget, step13WriteThroughOldData)
			before := step13WriteThroughSnapshot(t, root)
			calls := step13WriteThroughCalls{}
			var file *os.File
			var err error
			if kind == "ordinary-handle-without-write-through" {
				file, err = root.Open(step13WriteThroughSource)
			} else {
				file, err = step13WriteThroughOpen(root, directory, nil, &calls)
			}
			if err != nil {
				t.Fatal("mode-guard fixture:", err)
			}
			t.Cleanup(func() { _ = file.Close() })
			if kind == "closed-native-source" {
				if err := step13WriteThroughClose(file, &calls); err != nil {
					t.Fatal(err)
				}
			}
			err = step13WriteThroughRename(file, directory, &calls)
			if err == nil || calls.renames != 0 {
				t.Fatalf("mode guard reached actual rename: err=%v calls=%+v", err, calls)
			}
			if kind == "ordinary-handle-without-write-through" {
				if calls.queryStatus != nil || calls.mode&windows.FILE_WRITE_THROUGH != 0 || calls.queries != 1 {
					t.Fatalf("ordinary-handle mode evidence: %+v", calls)
				}
				if err := step13WriteThroughClose(file, &calls); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, windows.ERROR_INVALID_HANDLE) || calls.queryStatus == nil || calls.queries != 2 || calls.mode != 0 {
				t.Fatalf("real failed query was mistaken for mode proof: err=%v calls=%+v", err, calls)
			}
			if !reflect.DeepEqual(before, step13WriteThroughSnapshot(t, root)) {
				t.Fatal("blocked mode guard changed namespace or payload")
			}
			t.Logf("%s real query=%s mode=%#x; required-mode guard refuses NtSetInformationFile calls=0; source/target full snapshot unchanged", kind, step13WriteThroughStatus(calls.queryStatus), calls.mode)
		})
	}
}

func TestFileBoundaryWriteThroughGateTargetSwapBoundary(t *testing.T) {
	root, directory, path := step13WriteThroughFixture(t)
	sourceInfo := step13WriteThroughWrite(t, root, step13WriteThroughSource, step13WriteThroughPayload)
	expectedTarget := step13WriteThroughWrite(t, root, step13WriteThroughTarget, step13WriteThroughOldData)
	calls := step13WriteThroughCalls{}
	file, err := step13WriteThroughOpen(root, directory, nil, &calls)
	if err != nil {
		t.Fatal("target-swap prerequisite:", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	checked, err := root.Lstat(step13WriteThroughTarget)
	if err != nil || !checked.Mode().IsRegular() || store.IsReparseInfo(checked) || !os.SameFile(expectedTarget, checked) {
		t.Fatal("initial expected-target check:", err)
	}
	outside := t.TempDir()
	externalPath := filepath.Join(outside, "target-B")
	if err := os.WriteFile(externalPath, []byte("foreign target B sentinel"), 0644); err != nil {
		t.Fatal(err)
	}
	external, err := os.OpenRoot(outside)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = external.Close() })
	before := step13WriteThroughSnapshot(t, external)
	// Deterministic adversarial fixture swap after a successful SameFile check.
	// A and B are both ordinary; no reparse or path-escape is needed.
	targetPath := filepath.Join(path, step13WriteThroughTarget)
	if err := os.Rename(targetPath, targetPath+".held"); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(externalPath, targetPath); err != nil {
		t.Fatal(err)
	}
	currentB, err := root.Lstat(step13WriteThroughTarget)
	outsideB, outsideErr := external.Lstat("target-B")
	if err != nil || outsideErr != nil || os.SameFile(expectedTarget, currentB) || !os.SameFile(currentB, outsideB) {
		t.Fatal("target B fixture identity:", err, outsideErr)
	}
	if err := step13WriteThroughRename(file, directory, &calls); err != nil {
		t.Fatal("traditional API unexpectedly blocked ordinary target B:", err)
	}
	if err := step13WriteThroughClose(file, &calls); err != nil {
		t.Fatal(err)
	}
	published, err := root.Lstat(step13WriteThroughTarget)
	heldA, heldErr := root.Lstat(step13WriteThroughTarget + ".held")
	data, readErr := root.ReadFile(step13WriteThroughTarget)
	if err != nil || heldErr != nil || readErr != nil || !os.SameFile(sourceInfo, published) || !os.SameFile(expectedTarget, heldA) || string(data) != step13WriteThroughPayload {
		t.Fatal("target-swap actual publication/retained A:", err, heldErr, readErr)
	}
	if !reflect.DeepEqual(before, step13WriteThroughSnapshot(t, external)) {
		t.Fatal("target-swap experiment changed outside B bytes/modes/full attrs/entries")
	}
	if calls.opens != 1 || calls.renames != 1 || calls.queries != 2 || calls.closes != 1 {
		t.Fatalf("target-swap actual calls=%+v", calls)
	}
	t.Logf("BLOCKED target-leaf boundary: expected A passed SameFile, ordinary B then replaced the leaf, traditional API successfully replaced current B instead of A; retained A identity and outside B full snapshot unchanged; no expected-target-FileID atomic condition; calls=%+v", calls)
}
