package sdk_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ww1489/seasprak/sdk"
)

func TestRejectedResourceSymlinkDoesNotWriteManifest(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	parent := filepath.Join(state, "sessions", "linked")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "resources")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		if out, e := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); e != nil {
			t.Fatalf("junction fixture: %v %s", e, out)
		}
	}
	s, err := sdk.CreateAgentSession(context.Background(), memoryOpts(t, t.TempDir(), state, "linked"))
	if s != nil {
		closeSession(t, s)
	}
	if err == nil {
		t.Error("resource symlink accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("creation wrote a manifest outside the state root")
	}
}

func TestRejectedSessionSymlinkDoesNotWriteManifest(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(state, "sessions", "linked")); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		if out, e := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(state, "sessions", "linked"), outside).CombinedOutput(); e != nil {
			t.Fatalf("junction fixture: %v %s", e, out)
		}
	}
	s, err := sdk.CreateAgentSession(context.Background(), memoryOpts(t, t.TempDir(), state, "linked"))
	if s != nil {
		closeSession(t, s)
	}
	if err == nil {
		t.Fatal("session symlink accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("rejected creation wrote a manifest outside the state root")
	}
}
