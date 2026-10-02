package web

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/ww1489/seasprak/internal/codeagent"
	product "github.com/ww1489/seasprak/internal/errors"
)

func attachmentCatalog(t *testing.T) (*Catalog, codeagent.Options, string) {
	t.Helper()
	opts, _ := catalogOptions(t)
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	res, err := c.Create(t.Context(), CatalogCreateRequest{IdempotencyKey: "s", Workspace: opts.Workspace, ModelRef: DefaultModelRef})
	if err != nil {
		t.Fatal(err)
	}
	return c, c.opts, res.Snapshot.SessionID
}
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if e, ok := product.AsError(err); !ok || e.Code != code {
		t.Fatalf("error=%v want %s", err, code)
	}
}
func TestAttachmentPublishesRecordLastAndReadsVerified(t *testing.T) {
	c, opts, sid := attachmentCatalog(t)
	rec, dup, err := c.SaveAttachment(t.Context(), sid, SaveAttachmentRequest{IdempotencyKey: "k", MimeType: "application/json", Name: "a.json", Content: []byte(`{"a":1}`)})
	if err != nil || dup || rec.Size != 7 {
		t.Fatalf("save: %v %+v", err, rec)
	}
	dir := filepath.Join(opts.StateRoot, "sessions", sid, "attachments")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("attachment dir has %d entries", len(entries))
	}
	got, data, err := c.ReadAttachment(t.Context(), sid, rec.ArtifactID)
	if err != nil || string(data) != `{"a":1}` || got.Name != "a.json" {
		t.Fatalf("read: %v", err)
	}
	orphan := c.attachmentID(sid, "orphan")
	if err = os.WriteFile(filepath.Join(dir, orphan+".bin"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.ReadAttachment(t.Context(), sid, orphan)
	wantCode(t, err, product.CodeNotFound)
	if err = os.WriteFile(filepath.Join(dir, rec.ArtifactID+".bin"), []byte(`{"a":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.ReadAttachment(t.Context(), sid, rec.ArtifactID)
	wantCode(t, err, product.CodeStorageUnavailable)
}
func TestAttachmentReadNeverWrites(t *testing.T) {
	c, opts, sid := attachmentCatalog(t)
	_, _, err := c.ReadAttachment(t.Context(), sid, c.attachmentID(sid, "none"))
	wantCode(t, err, product.CodeNotFound)
	if _, err = c.GetMetadata(t.Context(), sid); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"attachments", "metadata.json"} {
		if _, err := os.Lstat(filepath.Join(opts.StateRoot, "sessions", sid, name)); !os.IsNotExist(err) {
			t.Fatalf("read created %s", name)
		}
	}
}
func TestAttachmentRejectsLinkedDirectory(t *testing.T) {
	c, opts, sid := attachmentCatalog(t)
	target := t.TempDir()
	link := filepath.Join(opts.StateRoot, "sessions", sid, "attachments")
	if goruntime.GOOS == "windows" {
		cmd, err := exec.LookPath("cmd")
		if err != nil {
			t.Skip("cmd unavailable for junction fixture")
		}
		if out, err := exec.Command(cmd, "/d", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink: %v %s", err, out)
		}
	} else if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.SaveAttachment(t.Context(), sid, SaveAttachmentRequest{IdempotencyKey: "k", MimeType: "text/plain", Content: []byte("x")})
	wantCode(t, err, product.CodeStorageUnavailable)
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatal("wrote through linked attachment directory")
	}
}
func TestAttachmentDefaultProfileWritesNothing(t *testing.T) {
	c, opts, sid := attachmentCatalog(t)
	c.opts.Profile = codeagent.ProfileDefault
	_, _, err := c.SaveAttachment(t.Context(), sid, SaveAttachmentRequest{IdempotencyKey: "k", MimeType: "text/plain", Content: []byte("x")})
	wantCode(t, err, product.CodeResourceUnavailable)
	_, _, err = c.SetMetadata(t.Context(), sid, SetMetadataRequest{IdempotencyKey: "k", Name: "n"})
	wantCode(t, err, product.CodeResourceUnavailable)
	for _, name := range []string{"attachments", "metadata.json"} {
		if _, err := os.Lstat(filepath.Join(opts.StateRoot, "sessions", sid, name)); !os.IsNotExist(err) {
			t.Fatalf("default profile created %s", name)
		}
	}
}
