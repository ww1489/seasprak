package storage

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

// Junction fixtures are required here: unavailable commands or creation errors
// fail these boundary tests rather than turning them into successful skips.
func fileBoundarySubdirDirectoryLink(t *testing.T, link, outside string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		command, err := exec.LookPath("cmd.exe")
		if err != nil {
			t.Fatal("junction command unavailable:", err)
		}
		if output, err := exec.Command(command, "/d", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Fatalf("junction fixture: %v: %s", err, output)
		}
	} else if err := os.Symlink(outside, link); err != nil {
		t.Fatal("directory symlink fixture:", err)
	}
}

// The per-call opener performs the real directory open after a deterministic
// replacement. It is the same core used by SessionSubdir, not a second resolver.
func TestFileBoundarySessionSubdirRejectsDirectoryReplacement(t *testing.T) {
	for _, component := range []string{"sessions", "session", "attachments"} {
		t.Run(component, func(t *testing.T) {
			state, outside := t.TempDir(), t.TempDir()
			const sid = "bound-subdir"
			dir, err := PrepareSessionDir(state, sid)
			if err != nil {
				t.Fatal(err)
			}
			attachmentDir := filepath.Join(dir, "attachments")
			if err := os.Mkdir(attachmentDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside unchanged"), 0644); err != nil {
				t.Fatal(err)
			}
			replacement, outsideDir := attachmentDir, outside
			switch component {
			case "sessions":
				replacement = filepath.Dir(dir)
				outsideDir = filepath.Join(outside, sid, "attachments")
			case "session":
				replacement = dir
				outsideDir = filepath.Join(outside, "attachments")
			}
			if err := os.MkdirAll(outsideDir, 0755); err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, outside)
			opens := 0
			target := component
			if target == "session" {
				target = sid
			}
			root, _, err := sessionSubdirWithOpenRoot(state, sid, "attachments", false, func(parent *os.Root, name string) (*os.Root, error) {
				if name == target {
					opens++
					if err := os.Rename(replacement, replacement+".held"); err != nil {
						t.Fatal("replace checked directory:", err)
					}
					fileBoundarySubdirDirectoryLink(t, replacement, outside)
					t.Cleanup(func() { _ = os.Remove(replacement) })
				}
				return parent.OpenRoot(name)
			})
			if root != nil {
				_ = root.Close()
			}
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStorageUnavailable {
				t.Errorf("replaced %s error=%v, want storage_unavailable", component, err)
			}
			if root != nil || opens != 1 {
				t.Errorf("replaced directory returned root=%v; target opens=%d", root != nil, opens)
			}
			if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
				t.Error("subdirectory access changed outside bytes, modes or entries")
			}
		})
	}
}

func TestFileBoundarySessionSubdirReadOnlyAndOwnedLifetime(t *testing.T) {
	state := t.TempDir()
	const sid = "subdir-lifetime"
	dir, err := PrepareSessionDir(state, sid)
	if err != nil {
		t.Fatal(err)
	}
	before := fileBoundaryTree(t, state)
	if root, _, err := SessionSubdir(state, sid, "attachments", false); root != nil || err == nil {
		if root != nil {
			_ = root.Close()
		}
		t.Fatal("missing readonly child accepted")
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, state)) {
		t.Fatal("readonly child lookup created or changed files")
	}
	root, path, err := SessionSubdir(state, sid, "attachments", true)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if path != filepath.Join(dir, "attachments") {
		t.Fatal("diagnostic subdirectory path changed")
	}
	if err := root.WriteFile("owned", []byte("caller owns this root"), 0600); err != nil {
		t.Fatal("returned child root was closed:", err)
	}
	session, path, err := SessionSubdir(state, sid, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if path != dir {
		t.Fatal("diagnostic session path changed")
	}
	if _, err := session.Stat("attachments/owned"); err != nil {
		t.Fatal("returned session root was closed:", err)
	}
	if root, _, err := SessionSubdir(state, "missing", "attachments", true); root != nil || err == nil {
		t.Fatal("missing session was created")
	}
	if _, err := os.Lstat(filepath.Join(state, "sessions", "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("child creation also created a missing session:", err)
	}
}
