//go:build windows

package jsonl

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ww1489/seasprak/internal/sessions/store"
)

// Junction creation does not require the symbolic-link privilege. This is an
// additional Windows reparse-point test, not a replacement for file symlinks.
func TestBlobRejectWindowsCheckpointJunction(t *testing.T) {
	command, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Skip("cmd.exe unavailable for Windows junction fixture")
	}
	s, b, _ := blobStore(t)
	outside := t.TempDir()
	link := filepath.Join(s.dir, "checkpoints")
	out, err := exec.Command(command, "/d", "/c", "mklink", "/J", link, outside).CombinedOutput()
	if err != nil {
		t.Fatalf("create junction fixture: %v: %s", err, out)
	}
	defer os.Remove(link)
	ref, err := b.Put(context.Background(), "blob", []byte("checkpoint"))
	if err == nil || ref != (store.BlobRef{}) {
		t.Fatal("junction accepted for blob storage")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("blob write escaped session through junction")
	}
}
