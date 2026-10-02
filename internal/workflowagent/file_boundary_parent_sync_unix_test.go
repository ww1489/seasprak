//go:build !windows

package workflowagent

import (
	"os"
	"sync"
	"testing"
)

// Search-only permission preserves Stat/Lstat and child identity checks while
// denying the new read-only directory descriptor used by actual SyncRoot.
func denyWorkflowParentSync(t *testing.T, parent *os.Root) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Fatal("NEEDS_CONTEXT: root privileges bypass the temporary directory permission fixture")
	}
	file, err := parent.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	restore := sync.OnceFunc(func() {
		if err := file.Chmod(info.Mode().Perm()); err != nil {
			t.Error("restore test-owned parent permission:", err)
		}
		if err := file.Close(); err != nil {
			t.Error("close test-owned restoration descriptor:", err)
		}
	})
	t.Cleanup(restore)
	if err := file.Chmod(0111); err != nil {
		t.Fatal(err)
	}
	return restore
}
