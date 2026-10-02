package storage

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

func storageDirectoryLink(t *testing.T, link, outside string) {
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

func TestCreationRegistryRejectsLinkedComponentsAndNonregularLock(t *testing.T) {
	for _, part := range []string{"catalog", "creations", "writer.lock"} {
		t.Run(part, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			link := filepath.Join(root, "catalog")
			if part != "catalog" {
				if err := os.Mkdir(link, 0700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(link, "creations")
			}
			if part == "writer.lock" {
				if err := os.Mkdir(link, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(link, part), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				storageDirectoryLink(t, link, outside)
			}
			r, err := OpenCreationRegistry(root)
			if err == nil {
				r.Close()
				t.Fatal("linked or nonregular registry path accepted")
			}
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStorageUnavailable {
				t.Fatalf("registry path error=%v", err)
			}
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
				t.Fatal("registry wrote through linked path")
			}
		})
	}
}

func TestSessionAttachmentResolverRejectsLinkedComponentsAndNonregularFiles(t *testing.T) {
	for _, part := range []string{"sessions", "session", "attachments", "record", "bytes"} {
		t.Run(part, func(t *testing.T) {
			root, outside, sid, aid := t.TempDir(), t.TempDir(), "attachment-session", "0123456789abcdef0123456789abcdef"
			dir, err := PrepareSessionDir(root, sid)
			if err != nil {
				t.Fatal(err)
			}
			switch part {
			case "sessions":
				if err = os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(filepath.Dir(dir)); err != nil {
					t.Fatal(err)
				}
				storageDirectoryLink(t, filepath.Dir(dir), outside)
			case "session":
				if err = os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				storageDirectoryLink(t, dir, outside)
			case "attachments":
				storageDirectoryLink(t, filepath.Join(dir, "attachments"), outside)
			case "record", "bytes":
				if err = os.Mkdir(filepath.Join(dir, "attachments"), 0700); err != nil {
					t.Fatal(err)
				}
				file := aid + ".json"
				if part == "bytes" {
					file = aid + ".bin"
					if err = os.WriteFile(filepath.Join(dir, "attachments", aid+".json"), []byte(`{"artifactId":"0123456789abcdef0123456789abcdef","mimeType":"text/plain","size":1,"sha256":"unused","digest":"fixture"}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err = os.Mkdir(filepath.Join(dir, "attachments", file), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err = ReadSessionAttachment(root, sid, aid); err == nil {
				t.Fatal("linked or nonregular attachment accepted")
			}
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
				t.Fatal("attachment resolver wrote through linked path")
			}
		})
	}
	for _, sub := range []string{"../outside", "..", "a/b", `a\b`} {
		root := t.TempDir()
		if _, err := PrepareSessionDir(root, "attachment-session"); err != nil {
			t.Fatal(err)
		}
		if r, _, err := SessionSubdir(root, "attachment-session", sub, true); err == nil {
			r.Close()
			t.Fatal("path-shaped subdirectory accepted")
		}
	}
}
