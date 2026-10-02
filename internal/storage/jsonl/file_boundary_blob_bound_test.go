package jsonl

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

func TestFileBoundaryBlobBoundStaleDiagnosticPath(t *testing.T) {
	for _, diagnostic := range []string{"missing", "foreign"} {
		t.Run(diagnostic, func(t *testing.T) {
			s, blobs, _ := blobStore(t)
			ctx := context.Background()
			seed := []byte("bound seed checkpoint")
			seedRef, err := blobs.Put(ctx, "blob", seed)
			if err != nil {
				t.Fatal(err)
			}
			resource := s.roots.Resource
			bound, err := resource.Stat(".")
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "journal.jsonl"), []byte("foreign journal sentinel"), 0644); err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, outside)
			s.dir = outside
			if diagnostic == "missing" {
				s.dir = filepath.Join(outside, "diagnostic-only-missing")
			}
			writes, syncs := 0, 0
			s.write = func(f *os.File, data []byte) (int, error) { writes++; return f.Write(data) }
			s.syncFile = func(f *os.File) error { syncs++; return f.Sync() }
			data := []byte("new checkpoint through retained resource")
			ref, putErr := blobs.Put(ctx, "blob", data)
			if putErr != nil || store.VerifyBlob(ref, data) != nil {
				t.Errorf("default Put must ignore diagnostic path: ref=%+v err=%v", ref, putErr)
			} else {
				info, err := resource.Stat(filepath.Join("checkpoints", ref.Hash+".bin"))
				if err != nil || !info.Mode().IsRegular() || info.Size() != ref.Size {
					t.Errorf("Put did not publish in retained resource: %v", err)
				}
				if retry, err := blobs.Put(ctx, "blob", data); err != nil || retry != ref {
					t.Errorf("bound publication retry: ref=%+v err=%v", retry, err)
				}
				current, err := resource.Stat(filepath.Join("checkpoints", ref.Hash+".bin"))
				if err != nil || !os.SameFile(info, current) {
					t.Error("retry replaced final blob:", err)
				}
				got, err := blobs.Get(ctx, "blob", ref)
				if err != nil || !bytes.Equal(got, data) {
					t.Errorf("Get new bound blob: %q %v", got, err)
				}
			}
			got, getErr := blobs.Get(ctx, "blob", seedRef)
			if getErr != nil || !bytes.Equal(got, seed) {
				t.Errorf("default Get must ignore diagnostic path: data=%q err=%v", got, getErr)
			}
			if writes != 1 || syncs != 1 {
				t.Errorf("actual new/retry/Get file calls: writes=%d syncs=%d, want 1/1", writes, syncs)
			}
			if s.syncDir != nil {
				t.Error("nil directory option must remain nil, selecting bound SyncRoot")
			}
			current, err := resource.Stat(".")
			if err != nil || !os.SameFile(bound, current) {
				t.Error("Blob closed or changed borrowed Resource root:", err)
			}
			if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
				t.Error("bound Blob I/O changed foreign bytes, modes or entries")
			}
		})
	}
}

func TestFileBoundaryBlobBoundRetainedResourceMove(t *testing.T) {
	for _, component := range []string{"session", "namespace"} {
		t.Run(component, func(t *testing.T) {
			s, blobs, _ := blobStore(t)
			resource := s.roots.Resource
			bound, err := resource.Stat(".")
			if err != nil {
				t.Fatal(err)
			}
			path := s.dir
			if component == "namespace" {
				path = filepath.Dir(path)
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside unchanged"), 0644); err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, outside)
			moveErr := os.Rename(path, path+".held")
			if moveErr != nil {
				if runtime.GOOS != "windows" || !errors.Is(moveErr, syscall.Errno(32)) && !errors.Is(moveErr, syscall.Errno(5)) {
					t.Fatal("move retained resource:", moveErr)
				}
				t.Log("Windows actually denied retained resource/ancestor rename; verifying unchanged binding")
			} else {
				fileBoundaryDirectoryLink(t, path, outside)
				t.Log("retained resource/ancestor actually moved and diagnostic path replaced")
			}
			data := []byte("checkpoint after attempted resource move")
			ref, err := blobs.Put(context.Background(), "blob", data)
			if err != nil || store.VerifyBlob(ref, data) != nil {
				t.Errorf("Put lost retained binding: ref=%+v err=%v", ref, err)
			} else if got, err := blobs.Get(context.Background(), "blob", ref); err != nil || !bytes.Equal(got, data) {
				t.Errorf("Get lost retained binding: data=%q err=%v", got, err)
			}
			current, err := resource.Stat(".")
			if err != nil || !os.SameFile(bound, current) {
				t.Error("Blob changed/closed borrowed resource:", err)
			}
			if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
				t.Error("Blob changed replacement directory")
			}
		})
	}
}

func TestFileBoundaryBlobBoundClosedResourceHasNoPathFallback(t *testing.T) {
	s, blobs, _ := blobStore(t)
	ctx := context.Background()
	data := []byte("closed retained resource")
	ref, err := blobs.Put(ctx, "blob", data)
	if err != nil {
		t.Fatal(err)
	}
	before := fileBoundaryTree(t, filepath.Join(s.dir, "checkpoints"))
	if err := s.roots.Resource.Close(); err != nil {
		t.Fatal(err)
	}
	writes, syncs := 0, 0
	s.write = func(f *os.File, data []byte) (int, error) { writes++; return f.Write(data) }
	s.syncFile = func(f *os.File) error { syncs++; return f.Sync() }
	got, err := blobs.Get(ctx, "blob", ref)
	requireBlobError(t, err, product.CodeStorageUnavailable)
	if got != nil {
		t.Fatal("closed Resource returned bytes by reopening path")
	}
	returned, err := blobs.Put(ctx, "blob", []byte("must not reopen"))
	requireBlobError(t, err, product.CodeStorageUnavailable)
	if returned != (store.BlobRef{}) || writes != 0 || syncs != 0 {
		t.Fatal("closed Resource returned ref or attempted write/sync")
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, filepath.Join(s.dir, "checkpoints"))) {
		t.Fatal("closed Resource fell back to diagnostic path")
	}
}

func TestFileBoundaryBlobBoundCheckpointChildWindow(t *testing.T) {
	for _, operation := range []string{"put", "get"} {
		for _, window := range []string{"before-open", "after-open"} {
			for _, replacement := range []string{"ordinary", "internal-link", "external-link"} {
				t.Run(operation+"/"+window+"/"+replacement, func(t *testing.T) {
					s, _, _ := blobStore(t)
					data := []byte("checked checkpoint child")
					ref, err := s.Put(context.Background(), "blob", data)
					if err != nil {
						t.Fatal(err)
					}
					outside := t.TempDir()
					if err := os.WriteFile(filepath.Join(outside, ref.Hash+".bin"), data, 0644); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(filepath.Join(s.dir, "other"), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(s.dir, "other", ref.Hash+".bin"), data, 0644); err != nil {
						t.Fatal(err)
					}
					outsideBefore := fileBoundaryTree(t, outside)
					otherBefore := fileBoundaryTree(t, filepath.Join(s.dir, "other"))
					opens, leaves, writes, syncs, directories := 0, 0, 0, 0, 0
					s.write = func(f *os.File, p []byte) (int, error) { writes++; return f.Write(p) }
					s.syncFile = func(f *os.File) error { syncs++; return f.Sync() }
					s.syncDir = func(string) error { directories++; return errors.New("must not reach directory sync") }
					var opened *os.Root
					var ordinaryBefore map[string]fileBoundaryEntry
					openChild := func(parent *os.Root, name string) (*os.Root, error) {
						opens++
						if parent != s.roots.Resource || name != "checkpoints" {
							t.Fatal("checkpoint child was not opened relative to retained Resource")
						}
						if window == "after-open" {
							var err error
							opened, err = parent.OpenRoot(name)
							if err != nil {
								t.Fatal(err)
							}
						}
						if err := parent.Rename(name, name+".held"); err != nil {
							t.Fatal("replace checked checkpoints child:", err)
						}
						switch replacement {
						case "ordinary":
							if err := parent.Mkdir(name, 0755); err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(filepath.Join(s.dir, name, ref.Hash+".bin"), data, 0644); err != nil {
								t.Fatal(err)
							}
							ordinaryBefore = fileBoundaryTree(t, filepath.Join(s.dir, name))
						case "internal-link":
							fileBoundaryDirectoryLink(t, filepath.Join(s.dir, name), filepath.Join(s.dir, "other"))
						case "external-link":
							fileBoundaryDirectoryLink(t, filepath.Join(s.dir, name), outside)
						}
						if window == "before-open" {
							var err error
							opened, err = parent.OpenRoot(name)
							return opened, err
						}
						return opened, nil
					}
					openFile := func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
						leaves++
						return openBlobFile(root, name, flags, mode)
					}
					if operation == "put" {
						returned, err := s.putWithOpen(context.Background(), "blob", []byte("new bound blob"), openChild, openFile)
						requireBlobError(t, err, product.CodeStorageUnavailable)
						if returned != (store.BlobRef{}) {
							t.Fatal("changed child returned usable ref")
						}
					} else {
						got, err := s.getWithOpen(context.Background(), "blob", ref, openChild, openFile)
						requireBlobError(t, err, product.CodeStorageUnavailable)
						if got != nil {
							t.Fatal("changed child returned bytes")
						}
					}
					if opens != 1 || leaves != 0 || writes != 0 || syncs != 0 || directories != 0 {
						t.Fatalf("child/leaf/write/file-sync/dir-sync=%d/%d/%d/%d/%d, want 1/0/0/0/0", opens, leaves, writes, syncs, directories)
					}
					if opened != nil {
						if _, err := opened.Stat("."); err == nil {
							t.Fatal("rejected owned child root remained open")
						}
					}
					boundStoreRootsLive(t, s.roots)
					if !reflect.DeepEqual(outsideBefore, fileBoundaryTree(t, outside)) || !reflect.DeepEqual(otherBefore, fileBoundaryTree(t, filepath.Join(s.dir, "other"))) {
						t.Fatal("rejected child changed outside/internal-link target bytes, modes or entries")
					}
					if ordinaryBefore != nil && !reflect.DeepEqual(ordinaryBefore, fileBoundaryTree(t, filepath.Join(s.dir, "checkpoints"))) {
						t.Fatal("rejected ordinary A/B replacement had side effects")
					}
				})
			}
		}
	}
}

func TestFileBoundaryBlobBoundReadLeafWindow(t *testing.T) {
	for _, operation := range []string{"get", "retry"} {
		for _, window := range []string{"before-open", "after-open"} {
			for _, replacement := range []string{"ordinary", "file-link", "directory"} {
				t.Run(operation+"/"+window+"/"+replacement, func(t *testing.T) {
					s, _, _ := blobStore(t)
					data := []byte("same bytes do not establish leaf identity")
					ref, err := s.Put(context.Background(), "blob", data)
					if err != nil {
						t.Fatal(err)
					}
					outside := t.TempDir()
					external := filepath.Join(outside, "same-content")
					if err := os.WriteFile(external, data, 0644); err != nil {
						t.Fatal(err)
					}
					before := fileBoundaryTree(t, outside)
					opens, directories := 0, 0
					var opened *os.File
					var child *os.Root
					s.syncDir = func(path string) error {
						directories++
						if path != s.dir {
							t.Fatal("failed leaf access continued to checkpoints sync")
						}
						return store.SyncRoot(s.roots.Resource)
					}
					s.write = func(*os.File, []byte) (int, error) {
						t.Error("read/retry wrote file")
						return 0, errors.New("unexpected write")
					}
					s.syncFile = func(*os.File) error { t.Error("read/retry synced file"); return errors.New("unexpected sync") }
					openChild := func(parent *os.Root, name string) (*os.Root, error) {
						var err error
						child, err = parent.OpenRoot(name)
						return child, err
					}
					openFile := func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
						opens++
						if flags != os.O_RDONLY || name != ref.Hash+".bin" {
							t.Fatal("read leaf was not the expected relative readonly name")
						}
						if window == "after-open" {
							var err error
							opened, err = openBlobFile(root, name, flags, mode)
							if err != nil {
								t.Fatal(err)
							}
						}
						if err := root.Rename(name, name+".held"); err != nil {
							t.Fatal("replace checked read leaf:", err)
						}
						path := filepath.Join(s.dir, "checkpoints", name)
						switch replacement {
						case "ordinary":
							if err := os.Link(external, path); err != nil {
								t.Fatal(err)
							}
						case "file-link":
							fileBoundaryFileLink(t, path, external)
						case "directory":
							if err := root.Mkdir(name, 0755); err != nil {
								t.Fatal(err)
							}
						}
						if window == "before-open" {
							var err error
							opened, err = openBlobFile(root, name, flags, mode)
							return opened, err
						}
						return opened, nil
					}
					wantDirectories := 0
					if operation == "get" {
						got, err := s.getWithOpen(context.Background(), "blob", ref, openChild, openFile)
						requireBlobError(t, err, product.CodeStorageUnavailable)
						if got != nil {
							t.Fatal("replaced leaf returned bytes")
						}
					} else {
						wantDirectories = 1 // Parent sync precedes the existing-blob read.
						returned, err := s.putWithOpen(context.Background(), "blob", data, openChild, openFile)
						requireBlobError(t, err, product.CodeStorageUnavailable)
						if returned != (store.BlobRef{}) {
							t.Fatal("replaced existing leaf returned ref")
						}
					}
					if opens != 1 || directories != wantDirectories {
						t.Fatalf("leaf opens=%d dir-sync=%d, want 1/%d", opens, directories, wantDirectories)
					}
					if opened != nil {
						boundStoreFileClosed(t, opened)
					}
					if _, err := child.Stat("."); err == nil {
						t.Fatal("read failure left owned child open")
					}
					boundStoreRootsLive(t, s.roots)
					if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
						t.Fatal("read replacement changed external bytes, modes or entries")
					}
				})
			}
		}
	}
}

func TestFileBoundaryBlobBoundTempIdentityBeforeWriteAndLink(t *testing.T) {
	for _, window := range []string{"after-open", "after-file-sync"} {
		t.Run(window, func(t *testing.T) {
			s, _, _ := blobStore(t)
			data := []byte("actual synced temporary file")
			outside := t.TempDir()
			external := filepath.Join(outside, "replacement")
			replacement := data
			if window == "after-open" {
				replacement = nil
			}
			if err := os.WriteFile(external, replacement, 0644); err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, outside)
			opens, writes, syncs, directories := 0, 0, 0, 0
			var opened *os.File
			var child *os.Root
			var held, temporary string
			var owned, foreign os.FileInfo
			s.syncDir = func(path string) error {
				directories++
				if path != s.dir {
					t.Fatal("changed temporary identity reached child sync")
				}
				return store.SyncRoot(s.roots.Resource)
			}
			swap := func(name string) {
				temporary = name
				var err error
				owned, err = opened.Stat()
				if err != nil {
					t.Fatal(err)
				}
				if err := child.Rename(name, name+".held"); err != nil {
					t.Fatal("replace actual temporary file:", err)
				}
				held = name + ".held"
				if err := os.Link(external, filepath.Join(s.dir, "checkpoints", name)); err != nil {
					t.Fatal(err)
				}
				foreign, err = child.Lstat(name)
				if err != nil || os.SameFile(owned, foreign) {
					t.Fatal("fixture did not install different temporary identity:", err)
				}
			}
			s.write = func(f *os.File, p []byte) (int, error) { writes++; return f.Write(p) }
			s.syncFile = func(f *os.File) error {
				syncs++
				if err := f.Sync(); err != nil {
					return err
				}
				if window == "after-file-sync" {
					swap(filepath.Base(f.Name()))
				}
				return nil
			}
			returned, err := s.putWithOpen(context.Background(), "blob", data, func(parent *os.Root, name string) (*os.Root, error) {
				var err error
				child, err = parent.OpenRoot(name)
				return child, err
			}, func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
				opens++
				if flags != os.O_CREATE|os.O_EXCL|os.O_WRONLY || mode != 0600 {
					t.Fatal("temporary file lost real exclusive create contract")
				}
				var err error
				opened, err = openBlobFile(root, name, flags, mode)
				if err != nil {
					return opened, err
				}
				if window == "after-open" {
					swap(name)
				}
				return opened, nil
			})
			requireBlobError(t, err, product.CodeStorageUnavailable)
			want := 0
			if window == "after-file-sync" {
				want = 1
			}
			if returned != (store.BlobRef{}) || opens != 1 || writes != want || syncs != want || directories != 1 {
				t.Fatalf("ref=%+v open/write/sync/dir-sync=%d/%d/%d/%d, want empty/1/%d/%d/1", returned, opens, writes, syncs, directories, want, want)
			}
			boundStoreFileClosed(t, opened)
			if _, err := child.Stat("."); err == nil {
				t.Fatal("temp failure left child root open")
			}
			entries, err := os.ReadDir(filepath.Join(s.dir, "checkpoints"))
			if err != nil || len(entries) != 2 {
				t.Error("cleanup deleted foreign temp name or created final/fallback:", entries, err)
			}
			blobCleanupFileUnchanged(t, filepath.Join(s.dir, "checkpoints", temporary), foreign, replacement)
			blobCleanupFileUnchanged(t, filepath.Join(s.dir, "checkpoints", held), owned, replacement)
			boundStoreRootsLive(t, s.roots)
			if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
				t.Fatal("temp identity failure changed external bytes, modes or entries")
			}
		})
	}
}

func TestFileBoundaryBlobBoundOpenErrorsCloseOwnedHandles(t *testing.T) {
	for _, fault := range []string{"child", "temp", "reader"} {
		t.Run(fault, func(t *testing.T) {
			s, _, _ := blobStore(t)
			data := []byte("opener-owned handle cleanup")
			ref, err := s.Put(context.Background(), "blob", data)
			if err != nil {
				t.Fatal(err)
			}
			var child *os.Root
			var file *os.File
			children, leaves := 0, 0
			openChild := func(parent *os.Root, name string) (*os.Root, error) {
				children++
				var err error
				child, err = parent.OpenRoot(name)
				if err == nil && fault == "child" {
					err = errors.New("child opener failure after actual open")
				}
				return child, err
			}
			openFile := func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
				leaves++
				var err error
				file, err = openBlobFile(root, name, flags, mode)
				if err == nil {
					err = errors.New("leaf opener failure after actual open")
				}
				return file, err
			}
			if fault == "temp" {
				returned, err := s.putWithOpen(context.Background(), "blob", []byte("new temporary leaf"), openChild, openFile)
				requireBlobError(t, err, product.CodeStorageUnavailable)
				if returned != (store.BlobRef{}) {
					t.Fatal("failed opener returned ref")
				}
			} else {
				got, err := s.getWithOpen(context.Background(), "blob", ref, openChild, openFile)
				requireBlobError(t, err, product.CodeStorageUnavailable)
				if got != nil {
					t.Fatal("failed opener returned bytes")
				}
			}
			wantLeaves := 1
			if fault == "child" {
				wantLeaves = 0
			}
			if children != 1 || leaves != wantLeaves {
				t.Fatalf("actual child/leaf calls=%d/%d", children, leaves)
			}
			if _, err := child.Stat("."); err == nil {
				t.Fatal("failed opener did not close owned child")
			}
			if file != nil {
				boundStoreFileClosed(t, file)
			}
			entries, err := os.ReadDir(filepath.Join(s.dir, "checkpoints"))
			if err != nil || len(entries) != 1 || entries[0].Name() != ref.Hash+".bin" {
				t.Fatal("failed opener retained temporary or fallback files:", entries, err)
			}
			boundStoreRootsLive(t, s.roots)
		})
	}
}

func TestFileBoundaryBlobBoundJournalRejectsSameIdentityLinkAndMissing(t *testing.T) {
	for _, replacement := range []string{"same-identity-link", "missing", "directory"} {
		t.Run(replacement, func(t *testing.T) {
			s, _, _ := blobStore(t)
			ctx := context.Background()
			data := []byte("journal binding must remain ordinary")
			ref, err := s.Put(ctx, "blob", data)
			if err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, filepath.Join(s.dir, "checkpoints"))
			// Release only the fixture handle so Windows can move its name;
			// retain the actual original journal, without changing production sharing.
			if err := s.journal.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.roots.Resource.Rename("journal.jsonl", "journal.held"); err != nil {
				t.Fatal(err)
			}
			s.journal, err = s.roots.Resource.OpenFile("journal.held", os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			switch replacement {
			case "same-identity-link":
				fileBoundaryFileLink(t, filepath.Join(s.dir, "journal.jsonl"), filepath.Join(s.dir, "journal.held"))
			case "directory":
				if err := s.roots.Resource.Mkdir("journal.jsonl", 0700); err != nil {
					t.Fatal(err)
				}
			}
			children, leaves, writes, syncs := 0, 0, 0, 0
			s.write = func(f *os.File, p []byte) (int, error) { writes++; return f.Write(p) }
			s.syncFile = func(f *os.File) error { syncs++; return f.Sync() }
			s.syncDir = func(string) error { syncs++; return errors.New("must not reach directory sync") }
			openChild := func(parent *os.Root, name string) (*os.Root, error) { children++; return parent.OpenRoot(name) }
			openFile := func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
				leaves++
				return openBlobFile(root, name, flags, mode)
			}
			got, err := s.getWithOpen(ctx, "blob", ref, openChild, openFile)
			requireBlobError(t, err, product.CodeStorageUnavailable)
			if got != nil {
				t.Fatal("invalid journal binding returned bytes")
			}
			returned, err := s.putWithOpen(ctx, "blob", []byte("must not publish"), openChild, openFile)
			requireBlobError(t, err, product.CodeStorageUnavailable)
			if returned != (store.BlobRef{}) || children != 0 || leaves != 0 || writes != 0 || syncs != 0 {
				t.Fatal("invalid journal binding returned ref or reached child/leaf/write/sync")
			}
			boundStoreRootsLive(t, s.roots)
			if !reflect.DeepEqual(before, fileBoundaryTree(t, filepath.Join(s.dir, "checkpoints"))) {
				t.Fatal("invalid journal binding changed checkpoint bytes, modes or entries")
			}
		})
	}
}

func TestFileBoundaryBlobBoundExplicitSyncOrderAndBorrowedRoots(t *testing.T) {
	roots := boundStoreRoots(t, t.TempDir(), "blob", true)
	var events []string
	var child *os.Root
	directorySync := func(path string) error {
		switch path {
		case roots.Path:
			events = append(events, "resource-sync")
			return store.SyncRoot(roots.Resource)
		case filepath.Join(roots.Path, "checkpoints"):
			events = append(events, "checkpoints-sync")
			return store.SyncRoot(child)
		default:
			t.Fatal("explicit hook received a changed diagnostic path")
			return os.ErrInvalid
		}
	}
	s, err := OpenBound("blob", roots, nil, store.Header{}, Options{SyncDir: directorySync})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !reflect.DeepEqual(events, []string{"resource-sync"}) || s.syncDir == nil {
		t.Fatal("explicit directory hook was not retained for header initialization")
	}
	s.write = func(f *os.File, p []byte) (int, error) {
		events = append(events, "write")
		if s.mu.TryLock() {
			s.mu.Unlock()
			t.Fatal("Blob write was outside Store.mu lifetime")
		}
		return f.Write(p)
	}
	s.syncFile = func(f *os.File) error { events = append(events, "file-sync"); return f.Sync() }
	children, leaves := 0, 0
	openChild := func(parent *os.Root, name string) (*os.Root, error) {
		children++
		if parent != roots.Resource || name != "checkpoints" {
			t.Fatal("Blob did not borrow Resource for finite checkpoints child")
		}
		if s.mu.TryLock() {
			s.mu.Unlock()
			t.Fatal("Blob borrowed root outside Store.mu lifetime")
		}
		var err error
		child, err = parent.OpenRoot(name)
		return child, err
	}
	openFile := func(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
		leaves++
		if root != child {
			t.Fatal("Blob leaf did not use the actual checked child")
		}
		return openBlobFile(root, name, flags, mode)
	}
	for _, data := range [][]byte{nil, []byte("explicit hook bytes")} {
		events, children, leaves = nil, 0, 0
		ref, err := s.putWithOpen(context.Background(), "blob", data, openChild, openFile)
		if err != nil || store.VerifyBlob(ref, data) != nil || children != 1 || leaves != 1 || !reflect.DeepEqual(events, []string{"resource-sync", "write", "file-sync", "checkpoints-sync"}) {
			t.Fatalf("first publication: ref=%+v err=%v children/leaves=%d/%d events=%v", ref, err, children, leaves, events)
		}
		if _, err := child.Stat("."); err == nil {
			t.Fatal("successful Put abandoned owned child root")
		}
		boundStoreRootsLive(t, roots)
		events, children, leaves = nil, 0, 0
		retry, err := s.putWithOpen(context.Background(), "blob", data, openChild, openFile)
		if err != nil || retry != ref || children != 1 || leaves != 1 || !reflect.DeepEqual(events, []string{"resource-sync", "checkpoints-sync"}) {
			t.Fatalf("publication retry: err=%v children/leaves=%d/%d events=%v", err, children, leaves, events)
		}
		events, children, leaves = nil, 0, 0
		got, err := s.getWithOpen(context.Background(), "blob", ref, openChild, openFile)
		if err != nil || !bytes.Equal(got, data) || children != 1 || leaves != 1 || len(events) != 0 {
			t.Fatalf("readonly Get path: err=%v children/leaves=%d/%d events=%v", err, children, leaves, events)
		}
		if _, err := child.Stat("."); err == nil {
			t.Fatal("successful Get abandoned owned child root")
		}
		boundStoreRootsLive(t, roots)
	}
}

func TestFileBoundaryBlobBoundSyncFailureRetriesNilDefault(t *testing.T) {
	for _, fault := range []string{"resource", "checkpoints"} {
		t.Run(fault, func(t *testing.T) {
			s, _, _ := blobStore(t)
			data := []byte("directory sync uncertainty")
			writes, files, dirs := 0, 0, 0
			var child *os.Root
			s.write = func(f *os.File, p []byte) (int, error) { writes++; return f.Write(p) }
			s.syncFile = func(f *os.File) error { files++; return f.Sync() }
			s.syncDir = func(path string) error {
				dirs++
				if path == s.dir {
					if fault == "resource" {
						return errors.New("resource sync uncertainty")
					}
					return store.SyncRoot(s.roots.Resource)
				}
				if path != filepath.Join(s.dir, "checkpoints") {
					t.Fatal("invalid explicit checkpoints diagnostic path")
				}
				return errors.New("checkpoints sync uncertainty")
			}
			openChild := func(parent *os.Root, name string) (*os.Root, error) {
				var err error
				child, err = parent.OpenRoot(name)
				return child, err
			}
			ref, err := s.putWithOpen(context.Background(), "blob", data, openChild, openBlobFile)
			requireBlobError(t, err, product.CodeStorageUnavailable)
			wantWrites, wantDirs := 0, 1
			if fault == "checkpoints" {
				wantWrites, wantDirs = 1, 2
			}
			if ref != (store.BlobRef{}) || writes != wantWrites || files != wantWrites || dirs != wantDirs {
				t.Fatalf("uncertain publication returned ref or continued I/O: ref=%+v calls=%d/%d/%d", ref, writes, files, dirs)
			}
			if _, err := child.Stat("."); err == nil {
				t.Fatal("directory failure retained owned checkpoints root")
			}
			boundStoreRootsLive(t, s.roots)
			// Nil restores the actual bound default. A stale path forces both
			// resource and child sync to use roots rather than a path hook.
			s.syncDir = nil
			s.dir = filepath.Join(t.TempDir(), "stale-diagnostic")
			ref, err = s.putWithOpen(context.Background(), "blob", data, openChild, openBlobFile)
			if err != nil || store.VerifyBlob(ref, data) != nil || writes != 1 || files != 1 || dirs != wantDirs {
				t.Fatalf("bound nil retry: ref=%+v err=%v calls=%d/%d/%d", ref, err, writes, files, dirs)
			}
			got, err := s.Get(context.Background(), "blob", ref)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("nil default retry blob unavailable:", err)
			}
			boundStoreRootsLive(t, s.roots)
		})
	}
}
