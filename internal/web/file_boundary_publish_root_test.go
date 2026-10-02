package web

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	store "github.com/ww1489/seasprak/internal/storage"
)

func TestFileBoundaryPublishRootUsesBoundDirectoryForSync(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	before, err := root.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	// The directory argument is diagnostic information, deliberately stale.
	// Successful file publication must sync this borrowed Root, not that path.
	stalePath := filepath.Join(t.TempDir(), "unavailable-directory")
	if err := publishFile(root, stalePath, "complete.bin", []byte("complete bound content")); err != nil {
		t.Fatalf("bound publication reopened a diagnostic path: %v", err)
	}
	data, err := root.ReadFile("complete.bin")
	if err != nil || !bytes.Equal(data, []byte("complete bound content")) {
		t.Fatal("bound publication lost complete bytes:", err)
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("publication closed or changed borrowed root:", err)
	}
	if _, err := os.Lstat(stalePath); !os.IsNotExist(err) {
		t.Fatal("publication created diagnostic directory:", err)
	}
}

func TestFileBoundaryAttachmentPublisherKeepsRecordLastWithStaleDiagnosticPath(t *testing.T) {
	c, opts, sid := attachmentCatalog(t)
	stalePath := filepath.Join(t.TempDir(), "unavailable-directory")
	aid := c.attachmentID(sid, "bound-publication")
	var published []string
	c.publish = func(root *os.Root, dir, name string, data []byte) error {
		if name == aid+".json" {
			if len(published) != 1 || published[0] != aid+".bin" {
				t.Fatal("record publication preceded complete content")
			}
			content, err := root.ReadFile(aid + ".bin")
			if err != nil || !bytes.Equal(content, []byte("attachment bound content")) {
				t.Fatal("record publication has no complete content:", err)
			}
		}
		if err := publishFile(root, stalePath, name, data); err != nil {
			return err
		}
		published = append(published, name)
		return nil
	}
	rec, duplicate, err := c.SaveAttachment(context.Background(), sid, SaveAttachmentRequest{
		IdempotencyKey: "bound-publication", MimeType: "text/plain", Name: "bound.txt", Content: []byte("attachment bound content"),
	})
	if err != nil || duplicate || rec.ArtifactID != aid {
		t.Fatalf("public SaveAttachment: duplicate=%v id=%s error=%v", duplicate, rec.ArtifactID, err)
	}
	if len(published) != 2 || published[0] != aid+".bin" || published[1] != aid+".json" {
		t.Fatalf("actual publication order=%v", published)
	}
	got, content, err := store.ReadSessionAttachment(opts.StateRoot, sid, aid)
	if err != nil || got != rec || !bytes.Equal(content, []byte("attachment bound content")) {
		t.Fatal("published attachment failed verified resolver:", err)
	}
}
