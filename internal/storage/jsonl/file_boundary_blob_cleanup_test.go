package jsonl

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

func blobCleanupFileUnchanged(t *testing.T, path string, original os.FileInfo, data []byte) {
	t.Helper()
	current, err := os.Lstat(path)
	if err != nil || !ordinaryBoundFile(current) || !os.SameFile(original, current) {
		t.Error("cleanup changed or deleted observed file identity:", err)
		return
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		t.Error("cleanup changed observed file bytes:", err)
	}
}

func TestFileBoundaryBlobCleanupOpenErrorOwnership(t *testing.T) {
	for _, fault := range []string{"owned", "foreign", "unknown-identity"} {
		t.Run(fault, func(t *testing.T) {
			s, _, _ := blobStore(t)
			var child *os.Root
			var file *os.File
			var name string
			var owned, foreign os.FileInfo
			children, leaves, writes, syncs, directories := 0, 0, 0, 0, 0
			replacement := []byte("foreign opener-error replacement")
			s.write = func(f *os.File, p []byte) (int, error) { writes++; return f.Write(p) }
			s.syncFile = func(f *os.File) error { syncs++; return f.Sync() }
			s.syncDir = func(path string) error {
				directories++
				if path != s.dir {
					t.Fatal("failed opener reached child directory sync")
				}
				return store.SyncRoot(s.roots.Resource)
			}
			ref, err := s.putWithOpen(context.Background(), "blob", []byte("must not write"), func(parent *os.Root, component string) (*os.Root, error) {
				children++
				var err error
				child, err = parent.OpenRoot(component)
				return child, err
			}, func(root *os.Root, component string, flags int, mode os.FileMode) (*os.File, error) {
				leaves++
				name = component
				var err error
				file, err = openBlobFile(root, component, flags, mode)
				if err != nil {
					t.Fatal(err)
				}
				owned, err = file.Stat()
				if err != nil {
					t.Fatal(err)
				}
				if fault == "foreign" {
					if err := root.Rename(name, name+".held"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(s.dir, "checkpoints", name), replacement, 0600); err != nil {
						t.Fatal(err)
					}
					foreign, err = root.Lstat(name)
					if err != nil || os.SameFile(owned, foreign) {
						t.Fatal("fixture did not change identity:", err)
					}
				}
				if fault == "unknown-identity" {
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
				}
				return file, errors.New("actual file plus opener error")
			})
			requireBlobError(t, err, product.CodeStorageUnavailable)
			if ref != (store.BlobRef{}) || children != 1 || leaves != 1 || writes != 0 || syncs != 0 || directories != 1 {
				t.Fatalf("ref=%+v child/leaf/write/file-sync/dir-sync=%d/%d/%d/%d/%d", ref, children, leaves, writes, syncs, directories)
			}
			boundStoreFileClosed(t, file)
			if _, err := child.Stat("."); err == nil {
				t.Fatal("opener error retained owned child root")
			}
			boundStoreRootsLive(t, s.roots)
			path := filepath.Join(s.dir, "checkpoints", name)
			wantEntries := 0
			switch fault {
			case "owned":
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Error("known owned temp was not cleaned:", err)
				}
			case "foreign":
				wantEntries = 2
				blobCleanupFileUnchanged(t, path, foreign, replacement)
				blobCleanupFileUnchanged(t, path+".held", owned, nil)
			case "unknown-identity":
				wantEntries = 1
				blobCleanupFileUnchanged(t, path, owned, nil)
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil || len(entries) != wantEntries {
				t.Error("unexpected temp/final/fallback entries:", entries, err)
			}
		})
	}
}

func TestFileBoundaryBlobCleanupNameReusedAfterRemove(t *testing.T) {
	for _, fault := range []string{"success", "same-identity-reuse", "child-sync-error", "cancel-during-child-sync"} {
		t.Run(fault, func(t *testing.T) {
			s, _, _ := blobStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			data, replacement := []byte("published source"), []byte("later unrelated temporary name")
			if fault == "same-identity-reuse" {
				replacement = data
			}
			var child *os.Root
			var file *os.File
			var temporary string
			var owned, foreign os.FileInfo
			children, leaves, writes, syncs, directories := 0, 0, 0, 0, 0
			s.write = func(f *os.File, p []byte) (int, error) { writes++; return f.Write(p) }
			s.syncFile = func(f *os.File) error {
				syncs++
				if err := f.Sync(); err != nil {
					return err
				}
				var err error
				owned, err = f.Stat()
				return err
			}
			s.syncDir = func(path string) error {
				directories++
				if path == s.dir {
					return store.SyncRoot(s.roots.Resource)
				}
				if path != filepath.Join(s.dir, "checkpoints") {
					t.Fatal("unexpected directory sync path")
				}
				if _, err := child.Lstat(temporary); !os.IsNotExist(err) {
					t.Fatal("explicit owned-temp Remove did not finish before child sync:", err)
				}
				if fault == "same-identity-reuse" {
					entries, err := os.ReadDir(path)
					if err != nil || len(entries) != 1 {
						t.Fatal("expected the actual linked final before name reuse:", entries, err)
					}
					if err := child.Link(entries[0].Name(), temporary); err != nil {
						t.Fatal(err)
					}
					foreign, err = child.Lstat(temporary)
					if err != nil || !os.SameFile(owned, foreign) {
						t.Fatal("fixture did not reuse actual final identity:", err)
					}
					return store.SyncRoot(child)
				}
				f, err := child.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.Write(replacement); err != nil {
					f.Close()
					t.Fatal(err)
				}
				foreign, err = f.Stat()
				closeErr := f.Close()
				if err != nil || closeErr != nil || os.SameFile(owned, foreign) {
					t.Fatal("reuse fixture failed:", err, closeErr)
				}
				if fault == "child-sync-error" {
					return errors.New("child sync uncertain after name reuse")
				}
				if fault == "cancel-during-child-sync" {
					cancel()
					return ctx.Err()
				}
				return store.SyncRoot(child)
			}
			ref, err := s.putWithOpen(ctx, "blob", data, func(parent *os.Root, name string) (*os.Root, error) {
				children++
				var err error
				child, err = parent.OpenRoot(name)
				return child, err
			}, func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
				leaves++
				temporary = name
				var err error
				file, err = openBlobFile(root, name, flags, mode)
				return file, err
			})
			if fault == "success" || fault == "same-identity-reuse" {
				if err != nil || store.VerifyBlob(ref, data) != nil {
					t.Error("successful publication failed:", err)
				}
			} else {
				requireBlobError(t, err, product.CodeStorageUnavailable)
				if ref != (store.BlobRef{}) {
					t.Error("uncertain directory sync returned usable ref")
				}
			}
			if children != 1 || leaves != 1 || writes != 1 || syncs != 1 || directories != 2 {
				t.Fatalf("child/leaf/write/file-sync/dir-sync=%d/%d/%d/%d/%d", children, leaves, writes, syncs, directories)
			}
			boundStoreFileClosed(t, file)
			if _, err := child.Stat("."); err == nil {
				t.Fatal("name reuse left child root open")
			}
			boundStoreRootsLive(t, s.roots)
			blobCleanupFileUnchanged(t, filepath.Join(s.dir, "checkpoints", temporary), foreign, replacement)
			entries, err := os.ReadDir(filepath.Join(s.dir, "checkpoints"))
			if err != nil || len(entries) != 2 {
				t.Error("reused name removed or fallback created:", entries, err)
			}
			for _, entry := range entries {
				if entry.Name() != temporary {
					blobCleanupFileUnchanged(t, filepath.Join(s.dir, "checkpoints", entry.Name()), owned, data)
				}
			}
		})
	}
}

func TestFileBoundaryBlobCleanupOwnedPartialAndFailures(t *testing.T) {
	for _, fault := range []string{"short-write", "file-sync", "context"} {
		t.Run(fault, func(t *testing.T) {
			s, _, _ := blobStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var child *os.Root
			var file *os.File
			writes, syncs, directories := 0, 0, 0
			s.write = func(f *os.File, p []byte) (int, error) {
				writes++
				if fault == "short-write" {
					return f.Write(p[:2])
				}
				return f.Write(p)
			}
			s.syncFile = func(f *os.File) error {
				syncs++
				if err := f.Sync(); err != nil {
					return err
				}
				if fault == "file-sync" {
					return errors.New("actual file sync fault")
				}
				cancel()
				return nil
			}
			s.syncDir = func(path string) error {
				directories++
				if path != s.dir {
					t.Fatal("owned failure reached child sync")
				}
				return store.SyncRoot(s.roots.Resource)
			}
			ref, err := s.putWithOpen(ctx, "blob", []byte("partial source changes size"), func(parent *os.Root, name string) (*os.Root, error) {
				var err error
				child, err = parent.OpenRoot(name)
				return child, err
			}, func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
				var err error
				file, err = openBlobFile(root, name, flags, mode)
				return file, err
			})
			if fault == "context" {
				if !errors.Is(err, context.Canceled) {
					t.Error("context cancellation changed:", err)
				}
			} else {
				requireBlobError(t, err, product.CodeStorageUnavailable)
			}
			wantSyncs := 1
			if fault == "short-write" {
				wantSyncs = 0
			}
			if ref != (store.BlobRef{}) || writes != 1 || syncs != wantSyncs || directories != 1 {
				t.Fatal("owned failure returned ref or continued I/O")
			}
			boundStoreFileClosed(t, file)
			if _, err := child.Stat("."); err == nil {
				t.Fatal("owned failure retained child root")
			}
			boundStoreRootsLive(t, s.roots)
			entries, err := os.ReadDir(filepath.Join(s.dir, "checkpoints"))
			if err != nil || len(entries) != 0 {
				t.Error("owned partial or failed temp was retained:", entries, err)
			}
		})
	}
}
