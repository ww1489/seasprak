package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateResourceID(t *testing.T) {
	for _, id := range []string{"sess-reopen", "builtin-v1", "0123456789abcdef0123456789abcdef"} {
		if err := ValidateResourceID(id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	for _, id := range []string{"", ".", "..", "../x", `a/b`, `a\b`, "a:b", "CON", "com1", "NUL.txt", "LPT9", "foo..bar", " trailing"} {
		if err := ValidateResourceID(id); err == nil {
			t.Fatalf("%q was accepted", id)
		}
	}
}

func TestPathsOverlapIncludesFilesystemRoot(t *testing.T) {
	parent, err := ResolveDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := ResolveDir(filepath.VolumeName(parent) + string(os.PathSeparator))
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(parent, "child")
	sibling := filepath.Join(parent, "child-other")
	for _, dir := range []string{child, sibling} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, a, b string
		want       bool
	}{
		{"root-descendant", root, parent, true},
		{"descendant-root", parent, root, true},
		{"equal-root", root, root, true},
		{"parent-child", parent, child, true},
		{"child-parent", child, parent, true},
		{"equal-child", child, child, true},
		{"siblings-with-prefix", child, sibling, false},
		{"siblings-reverse", sibling, child, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PathsOverlap(tc.a, tc.b); got != tc.want {
				t.Fatalf("PathsOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	other, err := ResolveDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.VolumeName(root) != filepath.VolumeName(other) {
		if PathsOverlap(root, other) || PathsOverlap(other, root) {
			t.Fatal("directories on distinct volumes overlapped")
		}
	}
}

func TestPathsOverlapUsesRealDirectories(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	realParent, err := ResolveDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	realChild, err := ResolveDir(child)
	if err != nil {
		t.Fatal(err)
	}
	if !PathsOverlap(realParent, realChild) {
		t.Fatal("child directory was not inside the parent")
	}
	other := t.TempDir()
	realOther, err := ResolveDir(other)
	if err != nil {
		t.Fatal(err)
	}
	if PathsOverlap(realParent, realOther) {
		t.Fatal("distinct temp directories overlapped")
	}
	file := filepath.Join(parent, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDir(file); err == nil {
		t.Fatal("file was accepted as a directory")
	}
}
