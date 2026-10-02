package codeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestFileBoundaryManifestPublishBorrowedIgnoresDiagnostic(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	roots, err := storage.OpenResourceRoots(state, storage.ResourceCode, "publisher", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	before := fileBoundaryTree(t, outside)
	manifest := sampleManifest(t)
	for _, diagnostic := range []string{filepath.Join(outside, "missing"), outside} {
		if err := saveManifestForOptions(Options{StateRoot: diagnostic, SessionID: "publisher", fileRoot: roots.Resource}, manifest); err != nil {
			t.Fatalf("bound publication used diagnostic path: %v", err)
		}
		got, err := loadManifestFromRoot(roots.Resource, manifest.ID)
		if err != nil || !reflect.DeepEqual(got, manifest) {
			t.Fatalf("bound manifest=%+v error=%v", got, err)
		}
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
		t.Fatal("diagnostic directory changed")
	}
	if _, err := roots.Resource.Stat("."); err != nil {
		t.Fatal("borrowed root closed:", err)
	}
}

func TestFileBoundaryManifestPublishClosedBorrowHasNoFallback(t *testing.T) {
	state := t.TempDir()
	roots, err := storage.OpenResourceRoots(state, storage.ResourceCode, "publisher", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	if err := roots.Resource.Close(); err != nil {
		t.Fatal(err)
	}
	before := fileBoundaryTree(t, state)
	if err := saveManifestForOptions(Options{StateRoot: state, SessionID: "publisher", fileRoot: roots.Resource}, sampleManifest(t)); err == nil {
		t.Error("closed borrowed root fell back to the path writer")
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, state)) {
		t.Error("closed borrowed root wrote new path entries")
	}
}

func TestFileBoundaryManifestPublishCreateClosedBorrowReleasesBackend(t *testing.T) {
	opts := factoryDiskOptions(t)
	var backend *jsonl.Store
	var borrowed *os.Root
	calls := 0
	session, err := createAgentSessionWithOpen(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, options jsonl.Options) (*jsonl.Store, error) {
		calls++
		opened, err := jsonl.OpenBound(id, roots, file, header, options)
		if err != nil {
			return opened, err
		}
		backend, borrowed = opened, roots.Resource
		if err := borrowed.Close(); err != nil {
			t.Fatal(err)
		}
		return opened, nil
	})
	if session != nil {
		_ = session.Close(t.Context())
		t.Error("failed publication started session")
	}
	factoryRequireCode(t, err, product.CodeStorageUnavailable)
	if calls != 1 || backend == nil || backend.ResourceRoot() != nil {
		t.Error("failed Create leaked backend")
	}
	if opts.Model.(*testkit.FakeModel).Calls() != 0 {
		t.Error("failed Create invoked model")
	}
	if _, err := borrowed.Stat("."); !errors.Is(err, os.ErrClosed) {
		t.Error("borrowed root unexpectedly reopened:", err)
	}
	resources := filepath.Join(opts.StateRoot, "sessions", opts.SessionID, "resources")
	if _, err := os.Lstat(resources); !os.IsNotExist(err) {
		t.Error("failed borrowed publication wrote path resources:", err)
	}
	factoryAssertLegacyWriterReleased(t, opts)
}

func TestFileBoundaryManifestPublishPublicCreateFixedDefault(t *testing.T) {
	opts := factoryDiskOptions(t)
	var toolCalls atomic.Int32
	opts.Tools = []tools.Definition{{Name: "publisher-test", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { toolCalls.Add(1); return "unused", nil }}}
	session, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal("public Create:", err)
	}
	defer session.Close(t.Context())
	root := session.rt.opts.fileRoot
	if root == nil {
		t.Fatal("public Create did not retain default actual root")
	}
	got, err := loadManifestFromRoot(root, session.rt.generation)
	if err != nil || got.ID != session.rt.generation || got.Model != opts.GenerationFingerprint || len(got.Tools) != 1 {
		t.Fatal("fixed public caller did not save manifest:", err)
	}
	if opts.Model.(*testkit.FakeModel).Calls() != 0 || toolCalls.Load() != 0 {
		t.Fatal("public Create executed model/tools")
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
		t.Fatal("public session Close leaked owned root:", err)
	}
	factoryAssertLegacyWriterReleased(t, opts)
}

func TestFileBoundaryManifestPublishPlatformGuards(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "closed-root"} {
		t.Run(kind, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := root.WriteFile(".manifest-source", []byte("written source"), 0600); err != nil {
				t.Fatal(err)
			}
			file, err := root.Open(".manifest-source")
			if err != nil {
				t.Fatal(err)
			}
			written, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "foreign"), []byte("outside source"), 0644); err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, outside)
			if kind == "closed-root" {
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := root.Rename(".manifest-source", ".manifest-held"); err != nil {
					t.Fatal(err)
				}
				if kind == "directory" {
					if err := root.Mkdir(".manifest-source", 0700); err != nil {
						t.Fatal(err)
					}
				} else {
					fileBoundaryFileLink(t, filepath.Join(dir, ".manifest-source"), filepath.Join(outside, "foreign"))
				}
			}
			published, err := renameManifest(root, ".manifest-source", written)
			if published || err == nil {
				t.Fatal("platform helper accepted untrusted source")
			}
			if _, err := os.Lstat(filepath.Join(dir, "manifest.json")); !os.IsNotExist(err) {
				t.Fatal("rejected source was published:", err)
			}
			if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
				t.Fatal("guard changed outside bytes/modes/entries")
			}
		})
	}
}

func TestFileBoundaryManifestPublishChildFileErrorClosesRoots(t *testing.T) {
	for _, component := range []string{"sessions", "publisher", "resources", "generation"} {
		t.Run(component, func(t *testing.T) {
			manifest := sampleManifest(t)
			target := component
			if component == "generation" {
				target = manifest.ID
			}
			fault := errors.New("checked child returned root and error")
			var captured []*os.Root
			calls := 0
			err := saveStandaloneManifest(t.TempDir(), "publisher", manifest, &manifestPublishBoundaries{
				openChild: func(parent *os.Root, leaf string) (*os.Root, error) {
					child, err := parent.OpenRoot(leaf)
					if child != nil {
						captured = append(captured, child)
					}
					if err == nil && leaf == target {
						calls++
						return child, fault
					}
					return child, err
				},
			})
			if !errors.Is(err, fault) || calls != 1 {
				t.Fatal("child primary error changed:", err)
			}
			for _, root := range captured {
				if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
					t.Fatal("File+error child leaked:", err)
				}
			}
		})
	}
}

func manifestPublishFixture(t *testing.T) (*storage.ResourceRoots, capabilityManifest) {
	t.Helper()
	state := t.TempDir()
	old := sampleManifest(t)
	if err := saveManifest(state, "publisher", old); err != nil {
		t.Fatal(err)
	}
	roots, err := storage.OpenResourceRoots(state, storage.ResourceCode, "publisher", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = roots.Close() })
	return roots, old
}

func TestFileBoundaryManifestPublishStagesAndOwnership(t *testing.T) {
	for _, stage := range []string{"success", "open", "file-error", "unknown-file-error", "write", "short-write", "sync", "close", "context-close", "rename", "directory-sync"} {
		t.Run(stage, func(t *testing.T) {
			roots, old := manifestPublishFixture(t)
			candidate, err := buildManifest(old.ID, old.Tools, "new model identity")
			if err != nil {
				t.Fatal(err)
			}
			oldRaw, _ := json.Marshal(old)
			raw, _ := json.Marshal(candidate)
			fault := errors.New("publisher stage fault")
			if stage == "context-close" {
				fault = context.Canceled
			}
			var file *os.File
			var created, written os.FileInfo
			var generation *os.Root
			var children []*os.Root
			var name string
			openCalls, writeCalls, syncCalls, closeCalls, renameCalls, rootSyncCalls := 0, 0, 0, 0, 0, 0
			var order []string
			b := manifestPublishBoundaries{
				openChild: func(parent *os.Root, leaf string) (*os.Root, error) {
					child, err := parent.OpenRoot(leaf)
					if child != nil {
						children = append(children, child)
					}
					return child, err
				},
				openTemp: func(root *os.Root, leaf string) (*os.File, error) {
					openCalls++
					order = append(order, "open")
					generation, name = root, leaf
					if stage == "open" {
						if err := root.Mkdir(leaf, 0700); err != nil {
							t.Fatal(err)
						}
					}
					opened, err := root.OpenFile(leaf, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
					file = opened
					if err != nil {
						return opened, err
					}
					created, err = opened.Stat()
					if err != nil {
						t.Fatal(err)
					}
					if stage == "file-error" {
						return opened, fault
					}
					if stage == "unknown-file-error" {
						if err := opened.Close(); err != nil {
							t.Fatal(err)
						}
						return opened, fault
					}
					return opened, nil
				},
				write: func(file *os.File, payload []byte) (int, error) {
					writeCalls++
					order = append(order, "write")
					if stage == "write" || stage == "short-write" {
						payload = payload[:len(payload)/2]
					}
					n, err := file.Write(payload)
					if err != nil {
						return n, err
					}
					if stage == "write" {
						return n, fault
					}
					return n, nil
				},
				sync: func(file *os.File) error {
					syncCalls++
					order = append(order, "sync")
					if err := file.Sync(); err != nil {
						return err
					}
					if stage == "sync" {
						return fault
					}
					return nil
				},
				close: func(file *os.File) error {
					closeCalls++
					order = append(order, "close")
					written, err = file.Stat()
					if err != nil {
						t.Fatal(err)
					}
					if err := file.Close(); err != nil {
						return err
					}
					if stage == "close" || stage == "context-close" {
						return fault
					}
					return nil
				},
				rename: func(root *os.Root, leaf string, info os.FileInfo) (bool, error) {
					renameCalls++
					order = append(order, "rename")
					if !manifestWritten(info, written) || !os.SameFile(created, info) {
						t.Fatal("rename did not receive actual written identity")
					}
					published, err := renameManifest(root, leaf, info)
					if err != nil {
						return published, err
					}
					if stage == "rename" {
						return published, fault
					}
					return published, nil
				},
				syncRoot: func(root *os.Root) error {
					rootSyncCalls++
					order = append(order, "root-sync")
					if err := storage.SyncRoot(root); err != nil {
						return err
					}
					if rootSyncCalls == 1 && root != roots.Resource {
						t.Fatal("first sync lost actual session root")
					}
					if rootSyncCalls == 2 && root != children[0] {
						t.Fatal("second sync lost actual resources root")
					}
					if rootSyncCalls == 3 && root != generation {
						t.Fatal("final sync lost actual generation root")
					}
					if stage == "directory-sync" && rootSyncCalls == 3 {
						return fault
					}
					return nil
				},
			}
			err = saveManifestFromRoot(roots.Resource, candidate, &b)
			if stage == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if stage == "short-write" {
				if !errors.Is(err, io.ErrShortWrite) {
					t.Fatal("short write accepted:", err)
				}
			} else if stage == "open" {
				if err == nil {
					t.Fatal("exclusive opener accepted directory")
				}
			} else if !errors.Is(err, fault) {
				t.Fatal("primary stage error lost:", err)
			}
			if stage != "success" {
				if stage == "context-close" {
					if factoryError(err) != context.Canceled {
						t.Fatal("context error replaced")
					}
				} else {
					factoryRequireCode(t, factoryError(err), product.CodeStorageUnavailable)
				}
			}
			if file != nil && !errors.Is(file.Close(), os.ErrClosed) {
				t.Fatal("temp File ownership leaked")
			}
			for _, child := range children {
				if _, err := child.Stat("."); !errors.Is(err, os.ErrClosed) {
					t.Fatal("owned child root leaked:", err)
				}
			}
			if _, err := roots.Resource.Stat("."); err != nil {
				t.Fatal("borrowed session closed:", err)
			}
			gen, err := roots.Resource.OpenRoot("resources/" + old.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer gen.Close()
			final, err := gen.Lstat("manifest.json")
			got, readErr := gen.ReadFile("manifest.json")
			published := stage == "success" || stage == "rename" || stage == "directory-sync"
			want := oldRaw
			if published {
				want = raw
			}
			if err != nil || readErr != nil || !bytes.Equal(got, want) || !manifestOrdinary(final) {
				t.Fatal("incorrect final manifest state", err, readErr)
			}
			if published && (!manifestWritten(final, written) || final.Mode() != written.Mode()) {
				t.Fatal("publication lost identity, size or mode")
			}
			current, tempErr := gen.Lstat(name)
			if stage == "open" {
				if tempErr != nil || !current.IsDir() {
					t.Fatal("foreign directory removed")
				}
			} else if stage == "unknown-file-error" {
				if tempErr != nil || !os.SameFile(created, current) {
					t.Fatal("unverified identity removed")
				}
			} else if !os.IsNotExist(tempErr) {
				t.Fatal("own temp was not cleaned or renamed:", tempErr)
			}
			wantOrder := []string{"root-sync", "root-sync", "open"}
			if stage != "open" && stage != "file-error" && stage != "unknown-file-error" {
				wantOrder = append(wantOrder, "write")
			}
			if stage != "open" && stage != "file-error" && stage != "unknown-file-error" && stage != "write" && stage != "short-write" {
				wantOrder = append(wantOrder, "sync")
			}
			if stage == "success" || stage == "close" || stage == "context-close" || stage == "rename" || stage == "directory-sync" {
				wantOrder = append(wantOrder, "close")
			}
			if published {
				wantOrder = append(wantOrder, "rename")
			}
			if stage == "success" || stage == "directory-sync" {
				wantOrder = append(wantOrder, "root-sync")
			}
			if !reflect.DeepEqual(order, wantOrder) || openCalls != 1 || len(children) != 2 {
				t.Fatalf("actual calls order=%v want=%v children=%d", order, wantOrder, len(children))
			}
			t.Logf("actual stages open/write/sync/close/rename/root-sync=%d/%d/%d/%d/%d/%d; synthetic fault=%s; published=%v", openCalls, writeCalls, syncCalls, closeCalls, renameCalls, rootSyncCalls, stage, published)
		})
	}
}

func TestFileBoundaryManifestPublishWrittenSwapAndCleanup(t *testing.T) {
	for _, kind := range []string{"ordinary", "directory", "symlink", "partial-foreign"} {
		t.Run(kind, func(t *testing.T) {
			roots, manifest := manifestPublishFixture(t)
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "foreign"), []byte("foreign bytes"), 0644); err != nil {
				t.Fatal(err)
			}
			outsideBefore := fileBoundaryTree(t, outside)
			var gen *os.Root
			var name string
			var written, foreign os.FileInfo
			renames, writes := 0, 0
			fault := errors.New("partial own write fault")
			swap := func() {
				if err := gen.Rename(name, name+".held"); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "directory":
					if err := gen.Mkdir(name, 0755); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					fileBoundaryFileLink(t, filepath.Join(roots.Path, "resources", manifest.ID, name), filepath.Join(outside, "foreign"))
				default:
					if err := gen.WriteFile(name, []byte("foreign bytes"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				foreign, err = gen.Lstat(name)
				if err != nil {
					t.Fatal(err)
				}
			}
			b := manifestPublishBoundaries{
				openTemp: func(root *os.Root, leaf string) (*os.File, error) {
					gen, name = root, leaf
					return root.OpenFile(leaf, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				},
				write: func(file *os.File, raw []byte) (int, error) {
					writes++
					if kind != "partial-foreign" {
						return file.Write(raw)
					}
					n, err := file.Write(raw[:len(raw)/2])
					if err != nil {
						return n, err
					}
					written, err = file.Stat()
					if err != nil {
						t.Fatal(err)
					}
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
					swap()
					return n, fault
				},
				close: func(file *os.File) error {
					var err error
					written, err = file.Stat()
					if err != nil {
						t.Fatal(err)
					}
					if err := file.Close(); err != nil {
						return err
					}
					swap()
					return nil
				},
				rename: func(root *os.Root, leaf string, info os.FileInfo) (bool, error) {
					renames++
					return renameManifest(root, leaf, info)
				},
			}
			err := saveManifestFromRoot(roots.Resource, manifest, &b)
			if kind == "partial-foreign" {
				if !errors.Is(err, fault) {
					t.Fatal("partial write primary error lost:", err)
				}
			} else {
				factoryRequireCode(t, err, product.CodeInvalidArgument)
			}
			if renames != 0 || writes != 1 {
				t.Fatal("changed written identity reached rename")
			}
			check, err := roots.Resource.OpenRoot("resources/" + manifest.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer check.Close()
			current, err := check.Lstat(name)
			if err != nil || !os.SameFile(foreign, current) || current.Mode() != foreign.Mode() {
				t.Fatal("foreign temp name deleted or chmodded:", err)
			}
			held, err := check.Lstat(name + ".held")
			if err != nil || !os.SameFile(written, held) || held.Size() != written.Size() {
				t.Fatal("moved own source changed:", err)
			}
			if kind == "ordinary" || kind == "partial-foreign" {
				data, err := check.ReadFile(name)
				if err != nil || string(data) != "foreign bytes" {
					t.Fatal("foreign bytes changed:", err)
				}
			}
			if !reflect.DeepEqual(outsideBefore, fileBoundaryTree(t, outside)) {
				t.Fatal("cleanup changed outside")
			}
			got, err := loadManifestFromRoot(roots.Resource, manifest.ID)
			if err != nil || !reflect.DeepEqual(got, manifest) {
				t.Fatal("written replacement changed old manifest:", err)
			}
		})
	}
}

func TestFileBoundaryManifestPublishRenameReceivesWrittenID(t *testing.T) {
	roots, manifest := manifestPublishFixture(t)
	var foreign os.FileInfo
	var name string
	calls := 0
	err := saveManifestFromRoot(roots.Resource, manifest, &manifestPublishBoundaries{
		rename: func(root *os.Root, leaf string, written os.FileInfo) (bool, error) {
			calls++
			name = leaf
			if err := root.Rename(leaf, leaf+".held"); err != nil {
				t.Fatal(err)
			}
			if err := root.WriteFile(leaf, bytes.Repeat([]byte("x"), int(written.Size())), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			foreign, err = root.Lstat(leaf)
			if err != nil {
				t.Fatal(err)
			}
			return renameManifest(root, leaf, written)
		},
	})
	factoryRequireCode(t, err, product.CodeInvalidArgument)
	gen, err := roots.Resource.OpenRoot("resources/" + manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer gen.Close()
	current, err := gen.Lstat(name)
	if err != nil || !os.SameFile(foreign, current) || calls != 1 {
		t.Fatal("same-size foreign source published or removed:", err)
	}
	got, err := loadManifestFromRoot(roots.Resource, manifest.ID)
	if err != nil || !reflect.DeepEqual(got, manifest) {
		t.Fatal("platform helper accepted new sample instead of actual written ID:", err)
	}
}

func TestFileBoundaryManifestPublishSuccessReuseAndSyncFailure(t *testing.T) {
	roots, manifest := manifestPublishFixture(t)
	var name string
	var foreign os.FileInfo
	calls := 0
	fault := errors.New("actual generation sync uncertain")
	err := saveManifestFromRoot(roots.Resource, manifest, &manifestPublishBoundaries{
		rename: func(root *os.Root, leaf string, written os.FileInfo) (bool, error) {
			name = leaf
			published, err := renameManifest(root, leaf, written)
			if err != nil {
				return published, err
			}
			if err := root.WriteFile(leaf, []byte("reused foreign temp"), 0644); err != nil {
				t.Fatal(err)
			}
			foreign, err = root.Lstat(leaf)
			if err != nil {
				t.Fatal(err)
			}
			return published, nil
		},
		syncRoot: func(root *os.Root) error {
			calls++
			if err := storage.SyncRoot(root); err != nil {
				return err
			}
			if calls == 3 {
				return fault
			}
			return nil
		},
	})
	if !errors.Is(err, fault) || calls != 3 {
		t.Fatal("post-rename sync did not fail:", err)
	}
	gen, err := roots.Resource.OpenRoot("resources/" + manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer gen.Close()
	current, err := gen.Lstat(name)
	data, readErr := gen.ReadFile(name)
	if err != nil || readErr != nil || !os.SameFile(foreign, current) || current.Mode() != foreign.Mode() || string(data) != "reused foreign temp" {
		t.Fatal("post-rename cleanup removed reused name")
	}
	got, err := loadManifestFromRoot(roots.Resource, manifest.ID)
	if err != nil || !reflect.DeepEqual(got, manifest) {
		t.Fatal("uncertain sync rolled back publication:", err)
	}
}

func TestFileBoundaryManifestPublishParentSyncRetry(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			roots, err := storage.OpenResourceRoots(t.TempDir(), storage.ResourceCode, "publisher", true, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer roots.Close()
			manifest := sampleManifest(t)
			fault := errors.New("actual parent sync uncertain")
			firstCalls, secondCalls, opens := 0, 0, 0
			err = saveManifestFromRoot(roots.Resource, manifest, &manifestPublishBoundaries{
				syncRoot: func(root *os.Root) error {
					firstCalls++
					if err := storage.SyncRoot(root); err != nil {
						return err
					}
					if firstCalls == failAt {
						return fault
					}
					return nil
				},
				openTemp: func(root *os.Root, leaf string) (*os.File, error) {
					opens++
					return root.OpenFile(leaf, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				},
			})
			if !errors.Is(err, fault) || firstCalls != failAt || opens != 0 {
				t.Fatal("uncertain parent sync proceeded to temp write:", err)
			}
			if _, err := roots.Resource.Stat("resources"); err != nil {
				t.Fatal("prepared directory missing:", err)
			}
			err = saveManifestFromRoot(roots.Resource, manifest, &manifestPublishBoundaries{
				syncRoot: func(root *os.Root) error { secondCalls++; return storage.SyncRoot(root) },
			})
			if err != nil || secondCalls != 3 {
				t.Fatal("retry skipped actual parent sync:", err, secondCalls)
			}
		})
	}
}

func TestFileBoundaryManifestPublishStandaloneCheckedChildren(t *testing.T) {
	for _, component := range []string{"sessions", "session", "resources", "generation"} {
		for _, phase := range []string{"before-open", "after-open"} {
			for _, kind := range []string{"ordinary", "file", "link"} {
				t.Run(component+"/"+phase+"/"+kind, func(t *testing.T) {
					state, outside, sid := t.TempDir(), t.TempDir(), "publisher"
					manifest := sampleManifest(t)
					path := filepath.Join(state, "sessions", sid, "resources", manifest.ID)
					if err := os.MkdirAll(path, 0755); err != nil {
						t.Fatal(err)
					}
					target := manifest.ID
					switch component {
					case "sessions":
						path, target = filepath.Join(state, "sessions"), "sessions"
					case "session":
						path, target = filepath.Join(state, "sessions", sid), sid
					case "resources":
						path, target = filepath.Join(state, "sessions", sid, "resources"), "resources"
					}
					if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside unchanged"), 0644); err != nil {
						t.Fatal(err)
					}
					before := fileBoundaryTree(t, outside)
					var replacementBefore map[string]fileBoundaryEntry
					var children []*os.Root
					calls := 0
					swap := func() {
						if err := os.Rename(path, path+".held"); err != nil {
							t.Fatal(err)
						}
						switch kind {
						case "ordinary":
							if err := os.Mkdir(path, 0755); err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(filepath.Join(path, "sentinel"), []byte("foreign directory"), 0644); err != nil {
								t.Fatal(err)
							}
						case "file":
							if err := os.WriteFile(path, []byte("foreign file"), 0644); err != nil {
								t.Fatal(err)
							}
						case "link":
							manifestPublishDirectoryLink(t, path, outside)
						}
						replacementBefore = fileBoundaryTree(t, path)
					}
					err := saveStandaloneManifest(state, sid, manifest, &manifestPublishBoundaries{
						openChild: func(parent *os.Root, leaf string) (*os.Root, error) {
							if leaf == target {
								calls++
								if phase == "before-open" {
									swap()
								}
							}
							child, err := parent.OpenRoot(leaf)
							if child != nil {
								children = append(children, child)
							}
							if leaf == target && phase == "after-open" && err == nil {
								swap()
							}
							return child, err
						},
					})
					factoryRequireCode(t, err, product.CodeInvalidArgument)
					if calls != 1 {
						t.Fatal("checked child boundary not reached once")
					}
					for _, child := range children {
						if _, err := child.Stat("."); !errors.Is(err, os.ErrClosed) {
							t.Fatal("failed standalone publisher leaked child:", err)
						}
					}
					if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) || !reflect.DeepEqual(replacementBefore, fileBoundaryTree(t, path)) {
						t.Fatal("checked swap changed foreign bytes, modes or entries")
					}
				})
			}
		}
	}
}

func TestFileBoundaryManifestPublishCreateRealFailure(t *testing.T) {
	for _, kind := range []string{"resources-file", "target-directory"} {
		t.Run(kind, func(t *testing.T) {
			opts := factoryDiskOptions(t)
			var toolCalls atomic.Int32
			opts.Tools = []tools.Definition{{Name: "publisher-test", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { toolCalls.Add(1); return "unused", nil }}}
			var backend *jsonl.Store
			var namespace, borrowed *os.Root
			calls := 0
			session, err := createAgentSessionWithOpen(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, options jsonl.Options) (*jsonl.Store, error) {
				calls++
				opened, err := jsonl.OpenBound(id, roots, file, header, options)
				if err != nil {
					return opened, err
				}
				backend, namespace, borrowed = opened, roots.Namespace, roots.Resource
				if kind == "resources-file" {
					if err := borrowed.WriteFile("resources", []byte("foreign resource file"), 0644); err != nil {
						t.Fatal(err)
					}
				} else {
					binding, err := decodeBinding(header.Workspace)
					if err != nil {
						t.Fatal(err)
					}
					if err := borrowed.MkdirAll("resources/"+binding.Generation+"/manifest.json", 0700); err != nil {
						t.Fatal(err)
					}
				}
				return opened, nil
			})
			if session != nil {
				_ = session.Close(t.Context())
				t.Fatal("failed publication returned session")
			}
			code := product.CodeStorageUnavailable
			if kind == "resources-file" {
				code = product.CodeInvalidArgument
			}
			factoryRequireCode(t, err, code)
			if kind == "target-directory" {
				pe, _ := product.AsError(err)
				if pe.Message != "session factory storage is unavailable" {
					t.Fatal("raw rename error exposed")
				}
			}
			if calls != 1 || backend == nil || backend.ResourceRoot() != nil || opts.Model.(*testkit.FakeModel).Calls() != 0 || toolCalls.Load() != 0 {
				t.Fatal("failed Create leaked backend or invoked model/tools")
			}
			for _, root := range []*os.Root{namespace, borrowed} {
				if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
					t.Fatal("failed Create leaked owned root:", err)
				}
			}
			factoryAssertLegacyWriterReleased(t, opts)
		})
	}
}
