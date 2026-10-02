//go:build linux || darwin || windows

package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

// Test-only method prototype. Production access remains unchanged in Step 12.
func TestFileBoundaryRootPrototypeRetainsCheckedDirectory(t *testing.T) {
	for _, component := range []string{"sessions", "session", "attachments"} {
		t.Run(component, func(t *testing.T) {
			state, outside := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Join(state, "sessions", "boundary", "attachments"), 0700); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(state)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			parent, name, path := root, "sessions", filepath.Join(state, "sessions")
			if component != "sessions" {
				parent, err = fileBoundaryOpenCheckedRoot(root, "sessions")
				if err != nil {
					t.Fatal(err)
				}
				defer parent.Close()
				name, path = "boundary", filepath.Join(path, "boundary")
			}
			if component == "attachments" {
				parent, err = fileBoundaryOpenCheckedRoot(parent, "boundary")
				if err != nil {
					t.Fatal(err)
				}
				defer parent.Close()
				name, path = "attachments", filepath.Join(path, "attachments")
			}
			if err := os.WriteFile(filepath.Join(path, "marker"), []byte("trusted"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "marker"), []byte("external"), 0644); err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, outside)
			bound, err := fileBoundaryOpenCheckedRoot(parent, name)
			if err != nil {
				t.Fatal(err)
			}
			defer bound.Close()
			// Go's relative Windows roots share DELETE; the initial trusted root
			// is not renamed. A child handle stays bound across this real rename.
			if err := os.Rename(path, path+".held"); err != nil {
				t.Fatal("rename retained child root:", err)
			}
			storageDirectoryLink(t, path, outside)
			t.Cleanup(func() { _ = os.Remove(path) })
			got, err := ReadRegular(bound, "marker", 32)
			if err != nil || !bytes.Equal(got, []byte("trusted")) {
				t.Fatalf("bound directory read=%q error=%v", got, err)
			}
			reopened, err := fileBoundaryOpenCheckedRoot(parent, name)
			if reopened != nil {
				_ = reopened.Close()
			}
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
				t.Errorf("checked relative reopen=%v, want invalid_argument", err)
			}
			if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
				t.Error("root prototype changed external bytes, modes or entries")
			}
		})
	}
}

func TestFileBoundaryRootPublicationPrototype(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	path := filepath.Join(state, "publication")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	trusted, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	defer trusted.Close()
	bound, err := fileBoundaryOpenCheckedRoot(trusted, "publication")
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	for name, content := range map[string]string{"first": "first payload", "second": "second payload"} {
		file, err := bound.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(content); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "final"), []byte("external sentinel"), 0644); err != nil {
		t.Fatal(err)
	}
	before := fileBoundaryTree(t, outside)
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	storageDirectoryLink(t, path, outside)
	t.Cleanup(func() { _ = os.Remove(path) })
	if err := bound.Link("first", "final"); err != nil {
		t.Fatal("relative non-replacing publication:", err)
	}
	first, err := bound.Lstat("first")
	if err != nil {
		t.Fatal(err)
	}
	final, err := bound.Lstat("final")
	if err != nil || !os.SameFile(first, final) {
		t.Fatal("relative link lost publication identity", err)
	}
	if err := bound.Link("second", "final"); !os.IsExist(err) {
		t.Fatalf("relative link replaced an existing target: %v", err)
	}
	got, err := ReadRegular(bound, "final", 64)
	if err != nil || string(got) != "first payload" {
		t.Fatalf("non-replacing payload=%q error=%v", got, err)
	}
	if err := bound.Rename("second", "final"); err != nil {
		t.Fatal("relative replacement:", err)
	}
	got, err = ReadRegular(bound, "final", 64)
	if err != nil || string(got) != "second payload" {
		t.Fatalf("replacement payload=%q error=%v", got, err)
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
		t.Error("relative publication changed external bytes, modes or entries")
	}
}

func fileBoundaryOpenCheckedRoot(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || fileBoundaryReparseInfo(before) {
		return nil, product.NewError(product.CodeInvalidArgument, "prototype directory has invalid type")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, openErr := child.Stat(".")
	current, currentErr := parent.Lstat(name)
	if openErr != nil || currentErr != nil || !opened.IsDir() || fileBoundaryReparseInfo(opened) || !current.IsDir() || fileBoundaryReparseInfo(current) || !os.SameFile(before, opened) || !os.SameFile(opened, current) {
		_ = child.Close()
		return nil, product.NewError(product.CodeInvalidArgument, "prototype directory identity changed")
	}
	return child, nil
}
