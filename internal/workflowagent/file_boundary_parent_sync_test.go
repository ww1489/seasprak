package workflowagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

// Cleanup runs after the explicit production-close assertions, including on Fatal.
func workflowParentSyncOwnChild(t *testing.T, child *os.Root) {
	t.Helper()
	t.Cleanup(func() { _ = child.Close() })
}

func TestWorkflowFileBoundaryParentSyncCreateFailureAndRetry(t *testing.T) {
	for _, phase := range []string{"state", "namespace"} {
		t.Run(phase, func(t *testing.T) {
			model := testkit.NewFake()
			var effects atomic.Int32
			opts := testOptions(t, modelThenTool(), model, &effects)
			outside := t.TempDir()
			outsideBefore := workflowFactoryTree(t, outside)
			for attempt := 0; attempt < 2; attempt++ {
				children, finals, denialProbes := 0, 0, 0
				var held []*os.Root
				var restore func()
				created, err := createWorkflowAgent(t.Context(), opts, func(parent *os.Root, name string) (*os.Root, error) {
					children++
					child, err := parent.OpenRoot(name)
					if err != nil {
						return child, err
					}
					workflowParentSyncOwnChild(t, child)
					held = append(held, parent, child)
					if phase == "state" && name == "workflow-runs" || phase == "namespace" && name == opts.RunID {
						restore = denyWorkflowParentSync(t, parent)
						opened, openErr := child.Stat(".")
						named, currentErr := parent.Lstat(name)
						if openErr != nil || currentErr != nil || !opened.IsDir() || storage.IsReparseInfo(opened) || !os.SameFile(opened, named) {
							t.Fatal("permission fixture interrupted the child identity check, not parent SyncRoot")
						}
						denialProbes++
						pe, ok := product.AsError(storage.SyncRoot(parent))
						if !ok || pe.Code != product.CodeStorageUnavailable || pe.Message != "directory sync failed" {
							t.Fatal("NEEDS_CONTEXT: actual parent SyncRoot did not fail under the temporary fixture")
						}
					}
					return child, nil
				}, func(id string, roots *storage.ResourceRoots, inspected *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
					finals++
					return jsonl.OpenBound(id, roots, inspected, header, opt)
				})
				if created != nil {
					owned := created
					t.Cleanup(func() { _ = owned.Close(context.Background()) })
				}
				if restore != nil {
					restore()
				}
				if created != nil {
					_ = created.Close(context.Background())
					t.Fatalf("Create succeeded despite actual parent SyncRoot denial: childOpen=%d finalBackend=%d denialProbe=%d", children, finals, denialProbes)
				}
				pe, ok := product.AsError(err)
				if !ok || pe.Code != product.CodeStorageUnavailable || pe.Message != "directory sync failed" {
					t.Fatalf("failure did not originate at the checked actual parent sync stage: direct=%t error=%v childOpen=%d finalBackend=%d denialProbe=%d", ok, err, children, finals, denialProbes)
				}
				want := 1
				if phase == "namespace" {
					want = 2
				}
				if children != want || finals != 0 || denialProbes != 1 || model.Calls() != 0 || effects.Load() != 0 {
					t.Fatal("sync failure opened a downstream writer or executed work")
				}
				for _, root := range held {
					if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
						t.Error("sync failure leaked an acquired root")
					}
				}
				path := filepath.Join(opts.StateRoot, "workflow-runs", opts.RunID)
				if phase == "state" {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatal("state sync failure created the resource")
					}
				} else if entries, err := os.ReadDir(path); err != nil || len(entries) != 0 {
					t.Fatal("namespace sync failure created a journal or writer lock")
				}
				if !reflect.DeepEqual(outsideBefore, workflowFactoryTree(t, outside)) {
					t.Error("sync failure modified unrelated external names")
				}
				t.Logf("attempt=%d childOpen=%d finalBackend=%d actualDeniedProbe=%d model=%d tool=%d", attempt+1, children, finals, denialProbes, model.Calls(), effects.Load())
			}
			created, err := CreateWorkflowAgent(t.Context(), opts)
			if created != nil {
				owned := created
				t.Cleanup(func() { _ = owned.Close(context.Background()) })
			}
			if err != nil {
				t.Fatal("public same-name retry failed after permission restoration:", err)
			}
			if err := created.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			opened, err := OpenWorkflowAgent(t.Context(), opts)
			if opened != nil {
				owned := opened
				t.Cleanup(func() { _ = owned.Close(context.Background()) })
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := opened.Close(t.Context()); err != nil || model.Calls() != 0 || effects.Load() != 0 {
				t.Fatal("public Create/Open executed work or lost writer ownership")
			}
		})
	}
}

func TestWorkflowFileBoundaryParentSyncReadOnlyDoesNotSync(t *testing.T) {
	model := testkit.NewFake()
	var effects atomic.Int32
	opts := testOptions(t, modelThenTool(), model, &effects)
	created, err := CreateWorkflowAgent(t.Context(), opts)
	if created != nil {
		owned := created
		t.Cleanup(func() { _ = owned.Close(context.Background()) })
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(opts.StateRoot, "workflow-runs", opts.RunID, "writer.lock")); err != nil {
		t.Fatal(err)
	}
	before := workflowFactoryTree(t, opts.StateRoot)
	var restorations []func()
	children, finals, denialProbes := 0, 0, 0
	opts.ReadOnly = true
	opts.Models, opts.Tools = nil, nil
	opened, err := openWorkflowAgent(t.Context(), opts, func(parent *os.Root, name string) (*os.Root, error) {
		children++
		child, err := parent.OpenRoot(name)
		if err != nil {
			return child, err
		}
		workflowParentSyncOwnChild(t, child)
		// Deny sync only after this relative child is open; Unix OpenRoot
		// itself requires read permission before acquiring the root.
		restore := denyWorkflowParentSync(t, parent)
		restorations = append(restorations, restore)
		denialProbes++
		if pe, ok := product.AsError(storage.SyncRoot(parent)); !ok || pe.Code != product.CodeStorageUnavailable {
			t.Fatal("NEEDS_CONTEXT: readonly fixture did not deny actual parent sync")
		}
		return child, nil
	}, func(id string, roots *storage.ResourceRoots, inspected *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
		finals++
		return jsonl.OpenBound(id, roots, inspected, header, opt)
	})
	if opened != nil {
		owned := opened
		t.Cleanup(func() { _ = owned.Close(context.Background()) })
	}
	for _, restore := range restorations {
		restore()
	}
	if err != nil {
		t.Fatal("checked readonly Open unexpectedly needed parent sync permission:", err)
	}
	snap, err := opened.Snapshot(t.Context())
	if err != nil || snap.State != "created" || snap.Revision != 1 {
		t.Fatal("readonly observation changed initialized state")
	}
	if err := opened.Close(t.Context()); err != nil || children != 2 || finals != 1 || denialProbes != 2 || model.Calls() != 0 || effects.Load() != 0 {
		t.Fatal("readonly observation changed backend counts or executed work")
	}
	opened, err = OpenWorkflowAgent(t.Context(), opts)
	if opened != nil {
		owned := opened
		t.Cleanup(func() { _ = owned.Close(context.Background()) })
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(t.Context()); err != nil || !reflect.DeepEqual(before, workflowFactoryTree(t, opts.StateRoot)) {
		t.Fatal("public readonly Open changed names, bytes, modes or created a lock")
	}
	t.Logf("readonly childOpen=%d actualDeniedProbes=%d finalBackend=%d model=%d tool=%d unchangedTree=true", children, denialProbes, finals, model.Calls(), effects.Load())
}
