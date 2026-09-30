//go:build !windows

package web

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUnixRejectsReadableStateRootWithoutChangingPermissions(t *testing.T) {
	c := testConfig(t)
	if err := os.Mkdir(c.StateRoot, 0700); err != nil {
		t.Fatal("mkdir failed")
	}
	if err := os.Chmod(c.StateRoot, 0755); err != nil {
		t.Fatal("chmod failed")
	}
	s, err := Start(context.Background(), c, nil)
	if err == nil {
		s.Close()
		s.Wait()
		t.Fatal("unsafe mode accepted")
	}
	info, err := os.Stat(c.StateRoot)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("startup changed existing permissions")
	}
	files, err := filepath.Glob(filepath.Join(c.StateRoot, "web-*.token"))
	if err != nil || len(files) != 0 {
		t.Fatal("unsafe startup published credentials")
	}
}
