package storage

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func parentSyncClosed(t *testing.T, roots ...*os.Root) {
	t.Helper()
	for _, root := range roots {
		if root == nil {
			continue
		}
		if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
			t.Error("construction retained an owned directory root")
		}
	}
}

func TestResourceRootsParentSyncOrderAndExistingCreate(t *testing.T) {
	for _, kind := range []ResourceType{ResourceCode, ResourceWorkflow} {
		t.Run(string(kind), func(t *testing.T) {
			state := t.TempDir()
			namespace, err := resourceNamespace(kind)
			if err != nil {
				t.Fatal(err)
			}
			var identity []os.FileInfo
			for attempt := 0; attempt < 2; attempt++ {
				before := fileBoundaryTree(t, state)
				var heldState, heldNamespace, heldResource *os.Root
				var order []string
				syncs := 0
				roots, err := openResourceRoots(state, kind, "parent-sync", true, func(parent *os.Root, name string) (*os.Root, error) {
					order = append(order, "open:"+name)
					child, err := parent.OpenRoot(name)
					if err == nil {
						t.Cleanup(func() { _ = child.Close() })
					}
					if name == namespace {
						heldState, heldNamespace = parent, child
					} else {
						if parent != heldNamespace {
							t.Error("resource was reopened through a different namespace")
						}
						heldResource = child
					}
					return child, err
				}, func(parent *os.Root) error {
					syncs++
					child, name, label := heldNamespace, namespace, "state"
					if syncs == 2 {
						child, name, label = heldResource, "parent-sync", "namespace"
						if parent != heldNamespace {
							t.Error("namespace sync did not borrow its checked parent")
						}
					} else if syncs != 1 || parent != heldState || heldResource != nil {
						t.Error("state sync did not precede the resource open")
					}
					opened, err := child.Stat(".")
					if err != nil {
						return err
					}
					named, err := parent.Lstat(name)
					if err != nil || !opened.IsDir() || IsReparseInfo(opened) || !os.SameFile(opened, named) {
						t.Error("parent sync did not follow the completed child identity check")
					}
					if err := SyncRoot(parent); err != nil {
						return err
					}
					order = append(order, "sync:"+label)
					return nil
				})
				if roots != nil {
					t.Cleanup(func() { _ = roots.Close() })
				}
				if err != nil {
					t.Fatal(err)
				}
				if syncs != 2 || !reflect.DeepEqual(order, []string{"open:" + namespace, "sync:state", "open:parent-sync", "sync:namespace"}) {
					t.Errorf("actual parent sync=%d order=%v, want checked child then actual parent twice", syncs, order)
				}
				parentSyncClosed(t, heldState)
				if roots.Namespace != heldNamespace || roots.Resource != heldResource {
					t.Error("successful construction lost owner transfer")
				}
				for i, root := range []*os.Root{roots.Namespace, roots.Resource} {
					info, err := root.Stat(".")
					if err != nil {
						t.Fatal(err)
					}
					if attempt == 0 {
						identity = append(identity, info)
					} else if !os.SameFile(identity[i], info) {
						t.Error("existing-create retry replaced a directory")
					}
				}
				if err := roots.Close(); err != nil {
					t.Fatal(err)
				}
				parentSyncClosed(t, heldResource, heldNamespace)
				if attempt == 1 && !reflect.DeepEqual(before, fileBoundaryTree(t, state)) {
					t.Error("existing-create retry changed modes, names or bytes")
				}
				t.Logf("attempt=%d actualSync=%d order=%v", attempt+1, syncs, order)
			}
		})
	}
}

func TestResourceRootsParentSyncFaultClosesAndRetryResyncs(t *testing.T) {
	for _, kind := range []ResourceType{ResourceCode, ResourceWorkflow} {
		for _, failAt := range []int{1, 2} {
			label := "state"
			if failAt == 2 {
				label = "namespace"
			}
			t.Run(string(kind)+"/"+label, func(t *testing.T) {
				state, outside := t.TempDir(), t.TempDir()
				if err := os.WriteFile(filepath.Join(outside, "marker"), []byte("external unchanged"), 0644); err != nil {
					t.Fatal(err)
				}
				outsideBefore := fileBoundaryTree(t, outside)
				namespace, _ := resourceNamespace(kind)
				fault := errors.New("synthetic uncertain parent sync receipt")
				var identity []os.FileInfo
				// Two failed calls with the same names prove existence does not
				// bypass the sync. Each fault follows the actual directory I/O.
				for attempt := 0; attempt < 3; attempt++ {
					var stateOwner, nsOwner, resourceOwner *os.Root
					opens, syncs := 0, 0
					roots, err := openResourceRoots(state, kind, "retry", true, func(parent *os.Root, name string) (*os.Root, error) {
						opens++
						child, err := parent.OpenRoot(name)
						if err == nil {
							t.Cleanup(func() { _ = child.Close() })
						}
						if name == namespace {
							stateOwner, nsOwner = parent, child
						} else {
							resourceOwner = child
						}
						return child, err
					}, func(parent *os.Root) error {
						syncs++
						want := stateOwner
						if syncs == 2 {
							want = nsOwner
						}
						if parent != want {
							t.Error("sync borrowed a different parent")
						}
						if err := SyncRoot(parent); err != nil {
							return err
						}
						if attempt < 2 && syncs == failAt {
							return fault
						}
						return nil
					})
					if roots != nil {
						t.Cleanup(func() { _ = roots.Close() })
						defer roots.Close()
					}
					if attempt < 2 {
						if roots != nil || err != fault || opens != failAt || syncs != failAt {
							t.Errorf("failed attempt=%d opens=%d actualSync=%d returnedRoots=%t, want %d/%d/false and original fault", attempt+1, opens, syncs, roots != nil, failAt, failAt)
						}
						if roots != nil {
							_ = roots.Close()
						}
						parentSyncClosed(t, resourceOwner, nsOwner, stateOwner)
					} else {
						if err != nil || roots == nil || opens != 2 || syncs != 2 {
							t.Fatal("successful retry did not sync both existing parents")
						}
						parentSyncClosed(t, stateOwner)
						_ = roots.Close()
						parentSyncClosed(t, resourceOwner, nsOwner)
					}
					for i, path := range []string{filepath.Join(state, namespace), filepath.Join(state, namespace, "retry")} {
						info, err := os.Lstat(path)
						if i == 1 && failAt == 1 && attempt < 2 {
							if !os.IsNotExist(err) {
								t.Error("state sync failure admitted resource creation")
							}
							continue
						}
						if err != nil || !info.IsDir() || IsReparseInfo(info) {
							t.Fatal("sync failure did not leave its checked preparation directory")
						}
						if attempt == 0 {
							identity = append(identity, info)
						} else if i < len(identity) && !os.SameFile(identity[i], info) {
							t.Error("retry replaced a preparation directory instead of resyncing")
						}
					}
					if !reflect.DeepEqual(outsideBefore, fileBoundaryTree(t, outside)) {
						t.Error("parent sync changed external names, modes or bytes")
					}
					t.Logf("attempt=%d opens=%d actualSync=%d returnedRoots=%t", attempt+1, opens, syncs, roots != nil)
				}
			})
		}
	}
}

func TestResourceRootsParentSyncReadOnlyZeroSideEffects(t *testing.T) {
	for _, kind := range []ResourceType{ResourceCode, ResourceWorkflow} {
		t.Run(string(kind), func(t *testing.T) {
			state := t.TempDir()
			namespace, _ := resourceNamespace(kind)
			if err := os.MkdirAll(filepath.Join(state, namespace, "readonly"), 0755); err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, state)
			opens, syncs := 0, 0
			roots, err := openResourceRoots(state, kind, "readonly", false, func(parent *os.Root, name string) (*os.Root, error) {
				opens++
				child, err := parent.OpenRoot(name)
				if err == nil {
					t.Cleanup(func() { _ = child.Close() })
				}
				return child, err
			}, func(*os.Root) error {
				syncs++
				return errors.New("read-only must not sync")
			})
			if roots != nil {
				t.Cleanup(func() { _ = roots.Close() })
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := roots.Close(); err != nil {
				t.Fatal(err)
			}
			if opens != 2 || syncs != 0 || !reflect.DeepEqual(before, fileBoundaryTree(t, state)) {
				t.Error("read-only root construction synced, created or chmodded")
			}
			t.Logf("readonly opens=%d sync=%d unchangedTree=true", opens, syncs)
		})
	}
}

func TestResourceRootsParentSyncRetainsParentAfterNamespaceExchange(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "external-marker"), []byte("external unchanged"), 0644); err != nil {
		t.Fatal(err)
	}
	outsideBefore := fileBoundaryTree(t, outside)
	var stateOwner, nsOwner *os.Root
	var namespaceIdentity os.FileInfo
	opens, syncs := 0, 0
	roots, err := openResourceRoots(state, ResourceCode, "bound", true, func(parent *os.Root, name string) (*os.Root, error) {
		opens++
		child, err := parent.OpenRoot(name)
		if err == nil {
			t.Cleanup(func() { _ = child.Close() })
		}
		if err == nil && name == "sessions" {
			stateOwner, nsOwner = parent, child
			namespaceIdentity, err = child.Stat(".")
		}
		return child, err
	}, func(parent *os.Root) error {
		syncs++
		if syncs == 1 && parent != stateOwner || syncs == 2 && parent != nsOwner {
			t.Error("sync reopened a diagnostic parent instead of borrowing the checked root")
		}
		if err := SyncRoot(parent); err != nil {
			return err
		}
		if syncs == 1 {
			// The namespace's check has completed. Move its name and install
			// a distinct ordinary directory, without changing the held root.
			if err := os.Rename(filepath.Join(state, "sessions"), filepath.Join(state, "sessions.held")); err != nil {
				return err
			}
			if err := os.Rename(outside, filepath.Join(state, "sessions")); err != nil {
				return err
			}
		}
		return nil
	})
	if roots != nil {
		t.Cleanup(func() { _ = roots.Close() })
	}
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	actual, err := roots.Namespace.Stat(".")
	if err != nil || !os.SameFile(namespaceIdentity, actual) || opens != 2 || syncs != 2 {
		t.Fatal("checked namespace binding was lost after a completed name exchange")
	}
	if _, err := os.Stat(filepath.Join(state, "sessions.held", "bound")); err != nil {
		t.Fatal("resource was not created through the original checked namespace")
	}
	if !reflect.DeepEqual(outsideBefore, fileBoundaryTree(t, filepath.Join(state, "sessions"))) {
		t.Error("bound creation or sync changed the external replacement's names, bytes or modes")
	}
	parentSyncClosed(t, stateOwner)
	t.Logf("actual childOpen=%d parentSync=%d namespaceIdentityRetained=true externalTreeUnchanged=true", opens, syncs)
}
