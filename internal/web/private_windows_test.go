//go:build windows

package web

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsProtectedDirectoryAndFileACL(t *testing.T) {
	c := testConfig(t)
	s, err := Start(context.Background(), c, nil)
	if err != nil {
		t.Fatal("start failed")
	}
	defer func() {
		s.Close()
		if s.Wait() != nil {
			t.Error("shutdown failed")
		}
	}()
	for _, p := range []string{c.StateRoot, s.TokenPath()} {
		if checkPrivate(p) != nil {
			t.Fatal("owner-only protected ACL missing")
		}
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal("read ACL failed")
		}
		dacl, _, err := sd.DACL()
		if err != nil || dacl == nil || dacl.AceCount != 2 {
			t.Fatal("expected only current user and SYSTEM ACEs")
		}
	}
}

func TestWindowsRejectsWorldReadableStateRootWithoutChmodRepair(t *testing.T) {
	c := testConfig(t)
	if err := privateDir(c.StateRoot); err != nil {
		t.Fatal("create private directory failed")
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal("test ACL failed")
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal("test DACL failed")
	}
	if err = windows.SetNamedSecurityInfo(c.StateRoot, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal("install test DACL failed")
	}
	// chmod(0600) cannot revoke the world ACE on Windows.
	if err = os.Chmod(c.StateRoot, 0700); err != nil {
		t.Fatal("chmod failed")
	}
	s, err := Start(context.Background(), c, nil)
	if err == nil {
		s.Close()
		s.Wait()
		t.Fatal("unsafe Windows ACL accepted")
	}
	if checkPrivate(c.StateRoot) == nil {
		t.Fatal("startup silently rewrote directory ACL")
	}
	files, err := filepath.Glob(filepath.Join(c.StateRoot, "web-*.token"))
	if err != nil || len(files) != 0 {
		t.Fatal("unsafe startup published credentials")
	}
}
