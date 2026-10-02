//go:build !windows

package codeagent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

func manifestPublishDirectoryLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal("directory symlink fixture:", err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}

func TestFileBoundaryManifestPublishUnixPermissionsAndOverwrite(t *testing.T) {
	state := t.TempDir()
	manifest := sampleManifest(t)
	path := filepath.Join(state, "sessions", "publisher", "resources", manifest.ID)
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "manifest.json"), []byte("old payload"), 0644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(filepath.Join(path, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := saveManifest(state, "publisher", manifest); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(state, "sessions"), filepath.Join(state, "sessions", "publisher"), filepath.Dir(path), path} {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("actual directory mode is not 0700:", err, dir)
		}
	}
	current, err := os.Stat(filepath.Join(path, "manifest.json"))
	if err != nil || current.Mode().Perm() != 0600 || os.SameFile(old, current) {
		t.Fatal("Unix overwrite failed to preserve source 0600 contract:", err)
	}
	got, err := loadManifest(state, "publisher", manifest.ID)
	if err != nil || !reflect.DeepEqual(got, manifest) {
		t.Fatal("legacy standalone overwrite lost payload:", err)
	}
}

func TestFileBoundaryManifestPublishCreateMovedBinding(t *testing.T) {
	opts := factoryDiskOptions(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside unchanged"), 0644); err != nil {
		t.Fatal(err)
	}
	before := fileBoundaryTree(t, outside)
	var borrowed *os.Root
	calls := 0
	path := filepath.Join(opts.StateRoot, "sessions", opts.SessionID)
	session, err := createAgentSessionWithOpen(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, options jsonl.Options) (*jsonl.Store, error) {
		calls++
		opened, err := jsonl.OpenBound(id, roots, file, header, options)
		if err != nil {
			return opened, err
		}
		borrowed = roots.Resource
		if err := os.Rename(path, path+".held"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
		return opened, nil
	})
	if session != nil {
		defer session.Close(t.Context())
	}
	if err != nil || session == nil {
		t.Fatalf("default Create lost the actual backend binding: %v", err)
	}
	got, err := loadManifestFromRoot(borrowed, session.rt.generation)
	if err != nil || got.ID != session.rt.generation || calls != 1 {
		t.Fatal("Create did not publish bound generation:", err)
	}
	if opts.Model.(*testkit.FakeModel).Calls() != 0 {
		t.Fatal("Create invoked model")
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
		t.Fatal("moved diagnostic path changed outside")
	}
}
