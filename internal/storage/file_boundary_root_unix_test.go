//go:build linux || darwin

package storage

import (
	"os"
	"path/filepath"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

func fileBoundaryReparseInfo(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }

func TestFileBoundaryRootPrototypeRejectsAllowedInRootLink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	allowed, err := root.OpenRoot("link")
	if err != nil {
		t.Fatal("os.Root's contained-link guarantee changed:", err)
	}
	if err := allowed.Close(); err != nil {
		t.Fatal(err)
	}
	checked, err := fileBoundaryOpenCheckedRoot(root, "link")
	if checked != nil {
		_ = checked.Close()
	}
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
		t.Fatalf("product link rejection prototype=%v", err)
	}
}
