//go:build linux || darwin || windows

package jsonl

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// These prototypes are test-only and do not replace flock in the Store.
func TestFileBoundaryHandleLockPrototype(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, err := root.OpenFile("writer.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	before, err := root.Lstat("writer.lock")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		t.Fatal("prototype lost checked lock identity", err)
	}
	ok, err := fileBoundaryTryHandleLock(file)
	if err != nil || !ok {
		t.Fatalf("handle lock=%v error=%v", ok, err)
	}
	current, err := root.Lstat("writer.lock")
	if err != nil || !os.SameFile(opened, current) {
		t.Fatal("prototype lock identity changed after lock", err)
	}
	path := filepath.Join(dir, "writer.lock")
	legacy := flock.New(path)
	ok, err = legacy.TryLock()
	if err != nil || ok {
		t.Fatalf("legacy lock interoperability: acquired=%v error=%v", ok, err)
	}
	if err := legacy.Unlock(); err != nil {
		t.Fatal(err)
	}
	fileBoundaryProbeLock(t, path, "busy")
	if err := fileBoundaryUnlockHandle(file); err != nil {
		t.Fatal("unlock prototype:", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal("close prototype:", err)
	}
	fileBoundaryProbeLock(t, path, "available")
	file, err = root.OpenFile("writer.lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	ok, err = fileBoundaryTryHandleLock(file)
	if err != nil || !ok {
		_ = file.Close()
		t.Fatal("relock prototype:", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	fileBoundaryProbeLock(t, path, "available")
	if _, err := fileBoundaryTryHandleLock(file); err == nil {
		t.Error("closed lock handle unexpectedly succeeded")
	}
	if err := fileBoundaryUnlockHandle(file); err == nil {
		t.Error("closed unlock handle error was lost")
	}
}

func TestFileBoundaryHandleLockNameReplacementPrototype(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, err := root.OpenFile("writer.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ok, err := fileBoundaryTryHandleLock(file)
	if err != nil || !ok {
		t.Fatal("lock original:", err)
	}
	defer fileBoundaryUnlockHandle(file)
	if err := root.Rename("writer.lock", "writer.held"); err != nil {
		t.Fatal("rename handle-locked file:", err)
	}
	fresh, err := root.OpenFile("writer.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	bound, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	current, err := root.Lstat("writer.lock")
	if err != nil || os.SameFile(bound, current) {
		t.Fatal("fixture did not replace lock identity", err)
	}
	fileBoundaryProbeLock(t, filepath.Join(dir, "writer.held"), "busy")
	fileBoundaryProbeLock(t, filepath.Join(dir, "writer.lock"), "available")
	// This PASS confirms a limitation: a handle lock alone cannot prevent a
	// same-user process from replacing the named file and acquiring a new inode.
}

func TestFileBoundaryHandleLockSubprocess(t *testing.T) {
	if os.Getenv("SEASPRAK_STEP12_LOCK_HELPER") != "1" {
		return
	}
	file, err := os.OpenFile(os.Getenv("SEASPRAK_STEP12_LOCK_PATH"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ok, err := fileBoundaryTryHandleLock(file)
	if err != nil {
		t.Fatal(err)
	}
	want := os.Getenv("SEASPRAK_STEP12_LOCK_EXPECT") == "available"
	if ok != want {
		t.Fatalf("child lock acquired=%v, want %v", ok, want)
	}
	if ok {
		if err := fileBoundaryUnlockHandle(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func fileBoundaryProbeLock(t *testing.T, path, expected string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFileBoundaryHandleLockSubprocess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SEASPRAK_STEP12_LOCK_HELPER=1", "SEASPRAK_STEP12_LOCK_PATH="+path, "SEASPRAK_STEP12_LOCK_EXPECT="+expected)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-process handle lock %s: %v: %s", expected, err, out)
	}
}
