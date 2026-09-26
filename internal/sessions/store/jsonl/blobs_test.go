package jsonl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// ERROR_PRIVILEGE_NOT_HELD is the documented Windows symlink privilege error.
// Other fixture failures must fail the test, including on Unix.
func checkBlobSymlink(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if runtime.GOOS == "windows" && (errors.Is(err, syscall.Errno(1314)) || errors.Is(err, os.ErrPermission)) {
		t.Skip("Windows symlink creation privilege unavailable")
	}
	t.Fatal(err)
}

func blobStore(t *testing.T) (*Store, store.CheckpointBlobs, string) {
	t.Helper()
	root := t.TempDir()
	s, e := Open("blob", root, store.Header{}, Options{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	b, ok := any(s).(store.CheckpointBlobs)
	if !ok {
		t.Fatal("store does not implement checkpoint blobs")
	}
	return s, b, root
}
func requireBlobError(t *testing.T, err error, code string) {
	t.Helper()
	var pe *product.Error
	if !errors.As(err, &pe) || pe.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestBlobFaults(t *testing.T) {
	for _, fault := range []string{"short-write", "file-sync", "dir-sync", "checkpoint-dir-sync"} {
		t.Run(fault, func(t *testing.T) {
			s, b, _ := blobStore(t)
			ctx := context.Background()
			data := []byte("fault checkpoint")
			calls := 0
			switch fault {
			case "short-write":
				s.write = func(f *os.File, p []byte) (int, error) { calls++; return f.Write(p[:len(p)/2]) }
			case "file-sync":
				s.syncFile = func(*os.File) error { calls++; return errors.New("injected") }
			default:
				s.syncDir = func(path string) error {
					if fault == "dir-sync" || filepath.Base(path) == "checkpoints" {
						calls++
						return errors.New("injected")
					}
					return store.SyncDir(path)
				}
			}
			ref, e := b.Put(ctx, "blob", data)
			requireBlobError(t, e, product.CodeStorageUnavailable)
			if ref != (store.BlobRef{}) || calls != 1 {
				t.Fatalf("failure published ref or wrong calls: %+v %d", ref, calls)
			}
			entries, _ := os.ReadDir(filepath.Join(s.dir, "checkpoints"))
			for _, entry := range entries {
				if filepath.Ext(entry.Name()) != ".bin" {
					t.Fatalf("temporary file retained: %s", entry.Name())
				}
			}
			s.write = nil
			s.syncFile = func(f *os.File) error { return f.Sync() }
			s.syncDir = store.SyncDir
			ref, e = b.Put(ctx, "blob", data)
			if e != nil {
				t.Fatal(e)
			}
			got, e := b.Get(ctx, "blob", ref)
			if e != nil || !bytes.Equal(got, data) {
				t.Fatalf("retry: %v", e)
			}
		})
	}
}
func TestBlobReopenReadOnly(t *testing.T) {
	s, b, root := blobStore(t)
	ctx := context.Background()
	data := []byte("restart checkpoint")
	ref, e := b.Put(ctx, "blob", data)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	ro, e := Open("blob", root, store.Header{}, Options{ReadOnly: true})
	if e != nil {
		t.Fatal(e)
	}
	defer ro.Close()
	r := any(ro).(store.CheckpointBlobs)
	ro.syncFile = func(*os.File) error { t.Error("readonly synced file"); return nil }
	ro.syncDir = func(string) error { t.Error("readonly synced directory"); return nil }
	ro.write = func(*os.File, []byte) (int, error) { t.Error("readonly wrote file"); return 0, nil }
	got, e := r.Get(ctx, "blob", ref)
	if e != nil || !bytes.Equal(got, data) {
		t.Fatalf("reopen Get: %v", e)
	}
	bad, e := r.Put(ctx, "blob", data)
	requireBlobError(t, e, product.CodePermissionDenied)
	if bad != (store.BlobRef{}) {
		t.Fatal("readonly returned ref")
	}
}
func TestBlobTamperAndNonRegular(t *testing.T) {
	for _, kind := range []string{"hash", "length", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			s, b, _ := blobStore(t)
			ctx := context.Background()
			data := []byte("checkpoint")
			ref, e := b.Put(ctx, "blob", data)
			if e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(s.dir, "checkpoints", ref.Hash+".bin")
			switch kind {
			case "hash":
				e = os.WriteFile(path, []byte("CHECKPOINT"), 0600)
			case "length":
				e = os.WriteFile(path, []byte("short"), 0600)
			case "directory":
				e = os.Remove(path)
				if e == nil {
					e = os.Mkdir(path, 0700)
				}
			case "symlink":
				if e = os.Remove(path); e != nil {
					t.Fatal(e)
				}
				target := filepath.Join(t.TempDir(), "outside")
				if e = os.WriteFile(target, data, 0600); e != nil {
					t.Fatal(e)
				}
				e = os.Symlink(target, path)
				checkBlobSymlink(t, e)
			}
			if e != nil {
				t.Fatal(e)
			}
			if got, e := b.Get(ctx, "blob", ref); e == nil || got != nil {
				t.Fatal("invalid blob read")
			}
			if r, e := b.Put(ctx, "blob", data); e == nil || r != (store.BlobRef{}) {
				t.Fatal("invalid blob overwritten")
			}
			if kind == "hash" || kind == "length" {
				got, e := os.ReadFile(path)
				if e != nil || bytes.Equal(got, data) {
					t.Fatal("tampered content overwritten")
				}
			}
		})
	}
}
func TestBlobConcurrentReaderSeesOnlyCompleteFile(t *testing.T) {
	s, b, root := blobStore(t)
	ro, err := Open("blob", root, store.Header{}, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	reader := any(ro).(store.CheckpointBlobs)
	data := bytes.Repeat([]byte("checkpoint"), 1024)
	ref := store.BlobRef{Hash: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data))}
	started, release := make(chan struct{}), make(chan struct{})
	s.write = func(f *os.File, p []byte) (int, error) {
		n, e := f.Write(p[:len(p)/2])
		close(started)
		<-release
		if e != nil {
			return n, e
		}
		m, e := f.Write(p[n:])
		return n + m, e
	}
	type outcome struct {
		ref store.BlobRef
		err error
	}
	done := make(chan outcome, 1)
	go func() { r, e := b.Put(context.Background(), "blob", data); done <- outcome{r, e} }()
	<-started
	got, readErr := reader.Get(context.Background(), "blob", ref)
	close(release)
	result := <-done
	requireBlobError(t, readErr, product.CodeNotFound)
	if got != nil {
		t.Fatal("partial blob visible")
	}
	if result.err != nil || result.ref != ref {
		t.Fatalf("Put: %+v", result)
	}
	got, err = reader.Get(context.Background(), "blob", ref)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("complete read: %v", err)
	}
}

func TestBlobMissingGetDoesNotCreate(t *testing.T) {
	s, b, _ := blobStore(t)
	ref := store.BlobRef{Hash: fmt.Sprintf("%x", sha256.Sum256(nil))}
	got, e := b.Get(context.Background(), "blob", ref)
	requireBlobError(t, e, product.CodeNotFound)
	if got != nil {
		t.Fatal("missing returned bytes")
	}
	if _, e = os.Lstat(filepath.Join(s.dir, "checkpoints")); !os.IsNotExist(e) {
		t.Fatal("Get created checkpoints directory")
	}
}
func TestBlobRejectCheckpointPath(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			s, b, _ := blobStore(t)
			path := filepath.Join(s.dir, "checkpoints")
			var e error
			if kind == "file" {
				e = os.WriteFile(path, []byte("not directory"), 0600)
			} else {
				e = os.Symlink(t.TempDir(), path)
				checkBlobSymlink(t, e)
			}
			if e != nil {
				t.Fatal(e)
			}
			if ref, e := b.Put(context.Background(), "blob", []byte("x")); e == nil || ref != (store.BlobRef{}) {
				t.Fatal("unsafe checkpoint directory accepted")
			}
		})
	}
}
