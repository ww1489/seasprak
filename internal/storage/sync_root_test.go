package storage

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

func TestSyncRootRetainsBoundDirectory(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	path := filepath.Join(state, "bound")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside unchanged"), 0644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	root, err := OpenChildRoot(parent, "bound", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	before, err := root.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	outsideBefore := fileBoundaryTree(t, outside)
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal("move retained directory:", err)
	}
	storageDirectoryLink(t, path, outside)
	t.Cleanup(func() { _ = os.Remove(path) })
	if err := SyncRoot(root); err != nil {
		t.Fatal("sync retained directory:", err)
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("SyncRoot closed or changed borrowed root:", err)
	}
	if !reflect.DeepEqual(outsideBefore, fileBoundaryTree(t, outside)) {
		t.Fatal("SyncRoot changed replacement directory")
	}
}

func TestSyncRootClosedAndNilKeepProductCode(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		root *os.Root
		code string
	}{{"closed", root, product.CodeStorageUnavailable}, {"nil", nil, product.CodeInvalidArgument}} {
		t.Run(tc.name, func(t *testing.T) {
			if pe, ok := product.AsError(SyncRoot(tc.root)); !ok || pe.Code != tc.code {
				t.Fatalf("SyncRoot must return direct %s", tc.code)
			}
		})
	}
}
