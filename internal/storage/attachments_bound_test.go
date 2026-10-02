package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

func TestSessionAttachmentBoundRootKeepsOriginalDirectory(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	const sid = "bound-attachment"
	const aid = "0123456789abcdef0123456789abcdef"
	dir, err := PrepareSessionDir(state, sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "attachments"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(outside, "attachments"), 0755); err != nil {
		t.Fatal(err)
	}
	fileBoundaryAttachment(t, filepath.Join(dir, "attachments"), aid, []byte("trusted attachment"))
	fileBoundaryAttachment(t, filepath.Join(outside, "attachments"), aid, []byte("external attachment"))
	roots, err := OpenResourceRoots(state, ResourceCode, sid, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	before := fileBoundaryTree(t, outside)
	if err := os.Rename(dir, dir+".held"); err != nil {
		t.Fatal("move bound resource:", err)
	}
	storageDirectoryLink(t, dir, outside)
	got, data, err := ReadSessionAttachmentFromRoot(roots.Resource, aid)
	if err != nil || got.ArtifactID != aid || !bytes.Equal(data, []byte("trusted attachment")) {
		t.Fatalf("bound attachment used another directory: id=%s err=%v", got.ArtifactID, err)
	}
	for _, root := range []*os.Root{roots.Namespace, roots.Resource} {
		if _, err := root.Stat("."); err != nil {
			t.Fatal("attachment read closed a borrowed root:", err)
		}
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
		t.Error("bound read changed external bytes, modes or entries")
	}
}

func TestSessionAttachmentBoundRootMissingNeverCreates(t *testing.T) {
	state := t.TempDir()
	roots, err := OpenResourceRoots(state, ResourceCode, "missing-attachment", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	before := fileBoundaryTree(t, state)
	for _, aid := range []string{"../invalid", "0123456789abcdef0123456789abcdef"} {
		_, data, err := ReadSessionAttachmentFromRoot(roots.Resource, aid)
		if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeNotFound || data != nil {
			t.Errorf("missing attachment did not reject without bytes: aid=%s err=%v", aid, err)
		}
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, state)) {
		t.Error("missing attachment read created or modified resources")
	}
}
