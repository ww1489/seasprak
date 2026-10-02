package storage

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResourceRootsParentSyncFatalCleanup(t *testing.T) {
	const argument = "--q1-roots-fatal="
	const intentional = "Q1 intentional Fatal after roots cleanup registration"
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, argument) {
			parentSyncFatalRoots(t, strings.TrimPrefix(arg, argument), intentional)
			return
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"code/create", "code/readonly", "workflow/create", "workflow/readonly"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestResourceRootsParentSyncFatalCleanup$", "-test.v", "-test.timeout=45s", "--", argument+name)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			text := string(output)
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || strings.Count(text, intentional) != 1 || strings.Count(text, ".go:") != 2 || !strings.Contains(text, "Q1_ROOTS_CLEANUP case="+name+" ") || !strings.Contains(text, "stateClosed=true ownersClosed=true identitiesRetained=true") {
				t.Fatalf("controlled Fatal did not prove pre-exit roots cleanup: error=%v output=%s", err, text)
			}
			for _, line := range strings.Split(text, "\n") {
				if at := strings.Index(line, "Q1_ROOTS_CLEANUP "); at >= 0 {
					t.Logf("expectedFatalExit=1 %s", line[at:])
				}
			}
		})
	}
}

func parentSyncFatalRoots(t *testing.T, name, intentional string) {
	t.Helper()
	parts := strings.Split(name, "/")
	if len(parts) != 2 {
		t.Fatal("invalid controlled roots failure case")
	}
	kind := ResourceCode
	if parts[0] == "workflow" {
		kind = ResourceWorkflow
	}
	state := t.TempDir()
	namespace, err := resourceNamespace(kind)
	if err != nil {
		t.Fatal(err)
	}
	create := parts[1] == "create"
	if !create {
		if err := os.MkdirAll(filepath.Join(state, namespace, "fatal-roots"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	var roots *ResourceRoots
	var held []*os.Root
	var identities []os.FileInfo
	opens, syncs := 0, 0
	t.Cleanup(func() {
		wantSyncs := 0
		if create {
			wantSyncs = 2
		}
		if roots == nil || opens != 2 || syncs != wantSyncs || len(identities) != 2 {
			t.Error("controlled roots factory did not complete the expected stages")
			return
		}
		for _, root := range held {
			if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
				t.Error("acquired root remained open before process exit")
				return
			}
		}
		for i, path := range []string{filepath.Join(state, namespace), filepath.Join(state, namespace, "fatal-roots")} {
			info, err := os.Lstat(path)
			if err != nil || !os.SameFile(identities[i], info) || identities[i].Mode() != info.Mode() {
				t.Error("roots cleanup changed checked directory identity or mode")
				return
			}
		}
		t.Logf("Q1_ROOTS_CLEANUP case=%s childOpen=%d actualSync=%d stateClosed=true ownersClosed=true identitiesRetained=true", name, opens, syncs)
	})
	roots, err = openResourceRoots(state, kind, "fatal-roots", create, func(parent *os.Root, childName string) (*os.Root, error) {
		opens++
		child, err := parent.OpenRoot(childName)
		if err != nil {
			return child, err
		}
		t.Cleanup(func() { _ = child.Close() })
		held = append(held, parent, child)
		info, err := child.Stat(".")
		if err != nil {
			t.Fatal("controlled child identity read failed")
		}
		identities = append(identities, info)
		return child, nil
	}, func(parent *os.Root) error {
		syncs++
		return SyncRoot(parent)
	})
	if roots != nil {
		t.Cleanup(func() { _ = roots.Close() })
	}
	if err != nil || roots == nil {
		t.Fatal("controlled roots factory failed before owner transfer")
	}
	t.Fatal(intentional)
}
