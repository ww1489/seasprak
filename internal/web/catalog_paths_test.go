package web

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

func catalogDirectoryLink(t *testing.T, link, outside string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		cmd, err := exec.LookPath("cmd")
		if err != nil {
			t.Skip("cmd unavailable for junction fixture")
		}
		if out, err := exec.Command(cmd, "/d", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Fatalf("junction fixture: %v %s", err, out)
		}
	} else if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogRejectsLinkedAndNonregularSessionHeadersWithoutWriting(t *testing.T) {
	for _, attack := range []string{"session", "journal"} {
		t.Run(attack, func(t *testing.T) {
			c, opts, sid := attachmentCatalog(t)
			if err := c.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(opts.StateRoot, "sessions", sid)
			journal := filepath.Join(dir, "journal.jsonl")
			before, err := os.ReadFile(journal)
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if attack == "session" {
				if err := os.Rename(dir, filepath.Join(outside, sid)); err != nil {
					t.Fatal(err)
				}
				catalogDirectoryLink(t, dir, filepath.Join(outside, sid))
				journal = filepath.Join(outside, sid, "journal.jsonl")
			} else {
				if err := os.Rename(journal, filepath.Join(outside, "journal.jsonl")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(journal, 0700); err != nil {
					t.Fatal(err)
				}
				journal = filepath.Join(outside, "journal.jsonl")
			}
			c, err = NewCatalog(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close(context.Background())
			if _, err := c.Writer(t.Context(), sid); err == nil {
				t.Fatal("writer accepted invalid session path")
			}
			if _, err := c.Snapshot(t.Context(), sid); err == nil {
				t.Fatal("snapshot accepted invalid session path")
			}
			if _, _, _, err := c.Subscribe(t.Context(), sid, 0, config.Limits{}); err == nil {
				t.Fatal("history accepted invalid session path")
			}
			if _, _, err := c.SaveAttachment(t.Context(), sid, SaveAttachmentRequest{IdempotencyKey: "new", MimeType: "text/plain", Content: []byte("x")}); err == nil {
				t.Fatal("upload accepted invalid session path")
			}
			if _, _, err := c.SetMetadata(t.Context(), sid, SetMetadataRequest{IdempotencyKey: "new", Name: "x"}); err == nil {
				t.Fatal("metadata accepted invalid session path")
			}
			after, err := os.ReadFile(journal)
			if err != nil || !bytes.Equal(before, after) || len(c.writers) != 0 {
				t.Fatal("path attack wrote or acquired a writer")
			}
		})
	}
}

func TestCatalogListRejectsLinkedSessionNamespace(t *testing.T) {
	opts, m := catalogOptions(t)
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "outside-session"), 0700); err != nil {
		t.Fatal(err)
	}
	catalogDirectoryLink(t, filepath.Join(opts.StateRoot, "sessions"), outside)
	list, err := c.List(t.Context(), CatalogListRequest{})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStorageUnavailable || len(list.Sessions) != 0 {
		t.Fatalf("linked namespace enumerated: list=%+v err=%v", list, err)
	}
	if m.Calls() != 0 || len(c.writers) != 0 {
		t.Fatal("list executed or acquired writer")
	}
}
