package storage

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

func TestResourceRootsFiniteNamespacesAndClose(t *testing.T) {
	for _, tc := range []struct {
		kind      ResourceType
		namespace string
	}{{ResourceCode, "sessions"}, {ResourceWorkflow, "workflow-runs"}, {"", "sessions"}} {
		t.Run(string(tc.kind), func(t *testing.T) {
			state := t.TempDir()
			roots, err := OpenResourceRoots(state, tc.kind, "bound", true, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer roots.Close()
			if roots.Path != filepath.Join(state, tc.namespace, "bound") {
				t.Errorf("diagnostic path=%q", roots.Path)
			}
			resourceInfo, err := roots.Resource.Stat(".")
			if err != nil {
				t.Fatal(err)
			}
			named, err := roots.Namespace.Lstat("bound")
			if err != nil || !os.SameFile(resourceInfo, named) {
				t.Fatal("resource is not bound to its namespace", err)
			}
			if err := roots.Close(); err != nil {
				t.Fatal(err)
			}
			if err := roots.Close(); err != nil {
				t.Fatal("repeated close:", err)
			}
			for _, root := range []*os.Root{roots.Resource, roots.Namespace} {
				if _, err := root.Stat("."); err == nil {
					t.Error("Close left an owned root usable")
				}
			}
		})
	}
}

func TestResourceRootsReadOnlyAndExistingCreatePreserveTree(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(map[bool]string{false: "readonly", true: "existing-create"}[create], func(t *testing.T) {
			state := t.TempDir()
			path := filepath.Join(state, "sessions", "bound")
			if err := os.MkdirAll(path, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "marker"), []byte("trusted"), 0644); err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, state)
			var initial *os.Root
			opens := 0
			roots, err := OpenResourceRoots(state, ResourceCode, "bound", create, func(parent *os.Root, name string) (*os.Root, error) {
				if name == "sessions" {
					initial = parent
				}
				opens++
				return parent.OpenRoot(name)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer roots.Close()
			if opens != 2 || initial == nil {
				t.Fatalf("relative child opens=%d, want 2", opens)
			}
			if _, err := initial.Stat("."); err == nil {
				t.Error("successful construction retained the initial state root")
			}
			if err := roots.Close(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, fileBoundaryTree(t, state)) {
				t.Error("opening existing roots changed bytes, modes or entries")
			}
		})
	}
}

func TestResourceRootsMissingAndInvalidInputsDoNotCreate(t *testing.T) {
	for _, tc := range []struct {
		kind   ResourceType
		id     string
		create bool
		code   string
	}{{ResourceCode, "bound", false, ""}, {ResourceWorkflow, "bound", false, ""}, {"other", "bound", true, product.CodeInvalidArgument}, {ResourceCode, "../escape", true, product.CodeInvalidArgument}, {ResourceCode, "NUL", true, product.CodeInvalidArgument}, {ResourceWorkflow, "", true, product.CodeInvalidArgument}} {
		t.Run(string(tc.kind)+"/"+tc.id, func(t *testing.T) {
			state := t.TempDir()
			before := fileBoundaryTree(t, state)
			opens := 0
			roots, err := OpenResourceRoots(state, tc.kind, tc.id, tc.create, func(parent *os.Root, name string) (*os.Root, error) {
				opens++
				return parent.OpenRoot(name)
			})
			if roots != nil {
				_ = roots.Close()
				t.Fatal("invalid or missing resource returned roots")
			}
			if tc.code == "" {
				if !os.IsNotExist(err) {
					t.Errorf("missing resource error=%v", err)
				}
			} else {
				resourceRootsError(t, err, tc.code)
			}
			if opens != 0 || !reflect.DeepEqual(before, fileBoundaryTree(t, state)) {
				t.Errorf("rejection changed state or opened children: opens=%d", opens)
			}
		})
	}
}

func TestResourceRootsStateRootConfigurationErrors(t *testing.T) {
	for _, kind := range []ResourceType{ResourceCode, ResourceWorkflow} {
		for _, create := range []bool{false, true} {
			for _, input := range []string{"empty", "missing", "file"} {
				t.Run(string(kind)+"/"+map[bool]string{false: "readonly", true: "create"}[create]+"/"+input, func(t *testing.T) {
					fixture := t.TempDir()
					stateRoot := ""
					message := "state root is required"
					if input != "empty" {
						stateRoot = filepath.Join(fixture, "state")
						message = "state root must be an existing directory"
					}
					if input == "file" {
						if err := os.WriteFile(stateRoot, []byte("not a state directory"), 0644); err != nil {
							t.Fatal(err)
						}
					}
					before := fileBoundaryTree(t, fixture)
					opens := 0
					roots, err := OpenResourceRoots(stateRoot, kind, "bound", create, func(parent *os.Root, name string) (*os.Root, error) {
						opens++
						return parent.OpenRoot(name)
					})
					if roots != nil {
						_ = roots.Close()
						t.Error("invalid state root returned roots")
					}
					if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument || pe.Message != message || pe.Details != nil || len(pe.Refs) != 0 {
						t.Errorf("error=%v, want fixed invalid_argument without details or refs", err)
					}
					if opens != 0 || !reflect.DeepEqual(before, fileBoundaryTree(t, fixture)) {
						t.Errorf("configuration rejection opened children or changed fixture: opens=%d", opens)
					}
				})
			}
		}
	}
}

func TestResourceRootsRejectStaticDirectoryTypes(t *testing.T) {
	for _, component := range []string{"sessions", "bound"} {
		for _, kind := range []string{"file", "link"} {
			t.Run(component+"/"+kind, func(t *testing.T) {
				state, outside := t.TempDir(), t.TempDir()
				path := filepath.Join(state, "sessions")
				if component == "bound" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(path, "bound")
				}
				if kind == "link" {
					storageDirectoryLink(t, path, outside)
					t.Cleanup(func() { _ = os.Remove(path) })
				} else if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
				before := fileBoundaryTree(t, outside)
				roots, err := OpenResourceRoots(state, ResourceCode, "bound", false, nil)
				if roots != nil {
					_ = roots.Close()
					t.Fatal("invalid directory returned roots")
				}
				resourceRootsError(t, err, product.CodeInvalidArgument)
				if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
					t.Error("rejection changed external bytes, modes or entries")
				}
			})
		}
	}
}

func TestResourceRootsRejectOrdinaryIdentityReplacement(t *testing.T) {
	for _, component := range []string{"sessions", "bound"} {
		for _, phase := range []string{"before-open", "after-open"} {
			t.Run(component+"/"+phase, func(t *testing.T) {
				state, replacement := t.TempDir(), t.TempDir()
				path := filepath.Join(state, "sessions")
				if err := os.MkdirAll(filepath.Join(path, "bound"), 0700); err != nil {
					t.Fatal(err)
				}
				if component == "bound" {
					path = filepath.Join(path, "bound")
				} else if err := os.Mkdir(filepath.Join(replacement, "bound"), 0700); err != nil {
					t.Fatal(err)
				}
				opens := 0
				var parents, children []*os.Root
				roots, err := OpenResourceRoots(state, ResourceCode, "bound", false, func(parent *os.Root, name string) (*os.Root, error) {
					parents = append(parents, parent)
					if name != component {
						return parent.OpenRoot(name)
					}
					opens++
					var child *os.Root
					var err error
					if phase == "after-open" {
						child, err = parent.OpenRoot(name)
						if err != nil {
							return nil, err
						}
					}
					if err := os.Rename(path, path+".held"); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(replacement, path); err != nil {
						t.Fatal(err)
					}
					info, err := parent.Lstat(name)
					if err != nil || !info.IsDir() || IsReparseInfo(info) {
						t.Fatal("ordinary replacement was not an ordinary directory", err)
					}
					if phase == "before-open" {
						child, err = parent.OpenRoot(name)
					}
					children = append(children, child)
					return child, err
				})
				if roots != nil {
					_ = roots.Close()
					t.Fatal("identity replacement returned roots")
				}
				resourceRootsError(t, err, product.CodeInvalidArgument)
				if opens != 1 {
					t.Errorf("target opens=%d, want 1", opens)
				}
				for _, root := range append(parents, children...) {
					if root != nil {
						if _, err := root.Stat("."); err == nil {
							t.Error("rejected construction left an acquired root usable")
						}
					}
				}
				if err := os.Rename(state, state+".released"); err != nil {
					t.Fatal("failed construction did not release state directory:", err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(state + ".released") })
			})
		}
	}
}

func TestResourceRootsRetainCompletedBinding(t *testing.T) {
	for _, component := range []string{"sessions", "bound"} {
		t.Run(component, func(t *testing.T) {
			state, replacement := t.TempDir(), t.TempDir()
			path := filepath.Join(state, "sessions", "bound")
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "marker"), []byte("trusted"), 0600); err != nil {
				t.Fatal(err)
			}
			roots, err := OpenResourceRoots(state, ResourceCode, "bound", false, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer roots.Close()
			if component == "sessions" {
				path = filepath.Dir(path)
			}
			renameErr := os.Rename(path, path+".held")
			if renameErr != nil {
				if runtime.GOOS != "windows" || component != "sessions" || !errors.Is(renameErr, os.ErrPermission) {
					t.Fatal(renameErr)
				}
				// Windows may refuse moving an ancestor with an opened descendant.
				// Assert that actual denial and unchanged binding, rather than skip
				// or claim that this namespace replacement happened.
				named, err := roots.Namespace.Lstat("bound")
				opened, openErr := roots.Resource.Stat(".")
				if err != nil || openErr != nil || !os.SameFile(named, opened) {
					t.Fatal("denied ancestor rename changed binding", err, openErr)
				}
				t.Log("Windows denied namespace rename with the resource root open; binding unchanged")
			} else {
				storageDirectoryLink(t, path, replacement)
				t.Cleanup(func() { _ = os.Remove(path) })
			}
			got, err := ReadRegular(roots.Resource, "marker", 32)
			if err != nil || string(got) != "trusted" {
				t.Fatalf("bound read=%q error=%v", got, err)
			}
		})
	}
}

func TestOpenChildRootRetainsCompletedNamespaceBinding(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	path := filepath.Join(state, "sessions")
	if err := os.MkdirAll(filepath.Join(path, "bound"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "bound", "marker"), []byte("trusted"), 0600); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	namespace, err := OpenChildRoot(parent, "sessions", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer namespace.Close()
	before := fileBoundaryTree(t, outside)
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	storageDirectoryLink(t, path, outside)
	t.Cleanup(func() { _ = os.Remove(path) })
	resource, err := OpenChildRoot(namespace, "bound", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close()
	got, err := ReadRegular(resource, "marker", 32)
	if err != nil || string(got) != "trusted" {
		t.Fatalf("retained namespace read=%q error=%v", got, err)
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
		t.Error("retained namespace access changed external bytes, modes or entries")
	}
}

func TestResourceRootsOpenFailureClosesAcquiredRoots(t *testing.T) {
	for _, component := range []string{"sessions", "bound"} {
		for _, failure := range []string{"open", "stat", "current-missing"} {
			t.Run(component+"/"+failure, func(t *testing.T) {
				state := t.TempDir()
				if err := os.MkdirAll(filepath.Join(state, "sessions", "bound"), 0700); err != nil {
					t.Fatal(err)
				}
				injected := errors.New("directory opener unavailable")
				var acquired []*os.Root
				roots, err := OpenResourceRoots(state, ResourceCode, "bound", false, func(parent *os.Root, name string) (*os.Root, error) {
					acquired = append(acquired, parent)
					child, err := parent.OpenRoot(name)
					if err != nil || name != component {
						return child, err
					}
					acquired = append(acquired, child)
					switch failure {
					case "open":
						return child, injected
					case "stat":
						_ = child.Close()
					case "current-missing":
						if err := parent.Rename(name, name+".held"); err != nil {
							t.Fatal(err)
						}
					}
					return child, nil
				})
				if roots != nil || err == nil {
					if roots != nil {
						_ = roots.Close()
					}
					t.Fatalf("failed construction roots=%v error=%v", roots, err)
				}
				if failure == "open" && !errors.Is(err, injected) {
					t.Errorf("ordinary IO error changed: %v", err)
				}
				if failure == "current-missing" {
					resourceRootsError(t, err, product.CodeInvalidArgument)
				}
				for _, root := range acquired {
					if _, err := root.Stat("."); err == nil {
						t.Error("failure left an acquired root usable")
					}
				}
			})
		}
	}
}

func TestOpenChildRootValidatesNamesAndExplicitCreation(t *testing.T) {
	state := t.TempDir()
	parent, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	for _, name := range []string{"", ".", "..", "../escape", "a/b", `a\b`, "C:escape"} {
		child, err := OpenChildRoot(parent, name, true, nil)
		if child != nil {
			_ = child.Close()
			t.Fatal("invalid child name returned a root")
		}
		resourceRootsError(t, err, product.CodeInvalidArgument)
	}
	if entries, err := os.ReadDir(state); err != nil || len(entries) != 0 {
		t.Fatal("invalid names created entries", err)
	}
	if child, err := OpenChildRoot(parent, "child", false, nil); child != nil || !os.IsNotExist(err) {
		t.Fatalf("read-only missing child root=%v error=%v", child, err)
	}
	child, err := OpenChildRoot(parent, "child", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := parent.Lstat("child")
	if err != nil || !info.IsDir() || IsReparseInfo(info) {
		t.Fatal("created child has invalid type", err)
	}
}

type resourceRootsUnknownInfo struct{ os.FileInfo }

func (resourceRootsUnknownInfo) Sys() any { return nil }

func TestResourceRootsReparseInfoUsesObservedMetadata(t *testing.T) {
	state := t.TempDir()
	info, err := os.Lstat(state)
	if err != nil {
		t.Fatal(err)
	}
	if IsReparseInfo(info) {
		t.Error("ordinary directory marked as reparse")
	}
	if runtime.GOOS == "windows" && !IsReparseInfo(resourceRootsUnknownInfo{info}) {
		t.Error("unknown Windows FileInfo.Sys was accepted")
	}
	path := filepath.Join(t.TempDir(), "linked")
	storageDirectoryLink(t, path, state)
	t.Cleanup(func() { _ = os.Remove(path) })
	info, err = os.Lstat(path)
	if err != nil || !IsReparseInfo(info) {
		t.Fatal("observed directory link was accepted", err)
	}
}

func resourceRootsError(t *testing.T, err error, code string) {
	t.Helper()
	if pe, ok := product.AsError(err); !ok || pe.Code != code {
		t.Errorf("error=%v, want %s", err, code)
	}
}
