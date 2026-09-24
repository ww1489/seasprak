package store

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
