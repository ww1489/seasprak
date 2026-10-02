//go:build windows

package codeagent

import (
	"os"
	goruntime "runtime"
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

// Only this temporary parent's DACL is changed. A separately owned handle
// keeps restoration rights even after the production root has been closed.
func denyCodeParentSync(t *testing.T, parent *os.Root) func() {
	t.Helper()
	name, err := windows.UTF16PtrFromString(parent.Name())
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal("NEEDS_CONTEXT: temporary DACL restoration rights unavailable:", err)
	}
	original, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatal(err)
	}
	dacl, _, err := original.DACL()
	if err != nil || dacl == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("NEEDS_CONTEXT: temporary parent DACL unavailable")
	}
	control, _, err := original.Control()
	if err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatal(err)
	}
	flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		flags = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	restore := sync.OnceFunc(func() {
		if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, flags, nil, nil, dacl, nil); err != nil {
			t.Error("restore exact test-owned parent DACL:", err)
		}
		goruntime.KeepAlive(original)
		restored, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil || restored.String() != original.String() {
			t.Error("restored temporary parent DACL differs from original")
		}
		if err := windows.CloseHandle(handle); err != nil {
			t.Error("close test-owned DACL restoration handle:", err)
		}
	})
	t.Cleanup(restore)
	world, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		t.Fatal(err)
	}
	var pin goruntime.Pinner
	pin.Pin(world)
	defer pin.Unpin()
	deny, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.FILE_WRITE_DATA,
		AccessMode:        windows.DENY_ACCESS,
		Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(world)},
	}}, dacl)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, deny, nil); err != nil {
		t.Fatal("NEEDS_CONTEXT: temporary deny DACL could not be installed:", err)
	}
	return restore
}
