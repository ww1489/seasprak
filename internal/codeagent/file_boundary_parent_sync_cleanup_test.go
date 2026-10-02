package codeagent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestFileBoundaryParentSyncFatalCleanup(t *testing.T) {
	const argument = "--q1-parent-sync-fatal="
	const intentional = "Q1 intentional Fatal after owned cleanup registration"
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, argument) {
			codeParentSyncFatalChild(t, strings.TrimPrefix(arg, argument), intentional)
			return
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{"readonly/agent"}
	for _, mode := range []string{"create", "readonly"} {
		for _, phase := range []string{"state", "namespace"} {
			for _, point := range []string{"before-permission", "after-probe"} {
				cases = append(cases, mode+"/"+phase+"/"+point)
			}
		}
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestFileBoundaryParentSyncFatalCleanup$", "-test.v", "-test.timeout=45s", "--", argument+name)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			text := string(output)
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || strings.Count(text, intentional) != 1 || strings.Count(text, ".go:") != 2 || !strings.Contains(text, "Q1_CLEANUP case="+name+" ") || !strings.Contains(text, "allRootsClosed=true permissionRestored=true identityRetained=true modeRetained=true model=0 tool=0") {
				t.Fatalf("controlled Fatal did not prove pre-exit cleanup: error=%v output=%s", err, text)
			}
			for _, line := range strings.Split(text, "\n") {
				if at := strings.Index(line, "Q1_CLEANUP "); at >= 0 {
					t.Logf("expectedFatalExit=1 %s", line[at:])
				}
			}
		})
	}
}

func codeParentSyncFatalChild(t *testing.T, name, intentional string) {
	t.Helper()
	parts := strings.Split(name, "/")
	if len(parts) < 2 || len(parts) > 3 {
		t.Fatal("invalid controlled failure case")
	}
	model := testkit.NewFake()
	var effects atomic.Int32
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "fatal-cleanup", Profile: ProfileMemory, Model: model, GenerationFingerprint: "parent-sync-q1", Tools: []tools.Definition{countedTool("probe", &effects, nil)}}
	if parts[0] == "readonly" {
		created, err := CreateAgentSession(t.Context(), opts)
		if created != nil {
			t.Cleanup(func() { _ = created.Close(context.Background()) })
		}
		if err != nil || created == nil {
			t.Fatal("readonly preparation failed")
		}
		if err := created.Close(t.Context()); err != nil {
			t.Fatal("readonly preparation close failed")
		}
		opts.ReadOnly = true
	}
	var held []*os.Root
	var paths []string
	var identities []os.FileInfo
	children, finals, probes := 0, 0, 0
	// Registered before every child/permission cleanup: the observer executes
	// last, while this test process is still alive, before TempDir removal.
	t.Cleanup(func() {
		wantChildren, wantFinals, wantProbes := 1, 0, 0
		if parts[1] == "namespace" || parts[1] == "agent" {
			wantChildren = 2
		}
		if parts[1] == "agent" {
			wantFinals = 1
		} else if parts[2] == "after-probe" {
			wantProbes = 1
		}
		if children != wantChildren || finals != wantFinals || probes != wantProbes || model.Calls() != 0 || effects.Load() != 0 {
			t.Error("controlled failure reached unexpected factory counts")
			return
		}
		for _, root := range held {
			if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
				t.Error("owned root was still open before process exit")
				return
			}
		}
		for i, path := range paths {
			root, err := os.OpenRoot(path)
			if err != nil {
				t.Error("restored parent could not be reopened before process exit")
				return
			}
			info, statErr := root.Stat(".")
			syncErr := storage.SyncRoot(root)
			closeErr := root.Close()
			if statErr != nil || syncErr != nil || closeErr != nil || !os.SameFile(identities[i], info) || identities[i].Mode() != info.Mode() {
				t.Error("permission, mode or identity was not restored before process exit")
				return
			}
		}
		t.Logf("Q1_CLEANUP case=%s childOpen=%d finalBackend=%d denialProbe=%d allRootsClosed=true permissionRestored=true identityRetained=true modeRetained=true model=0 tool=0", name, children, finals, probes)
	})
	openChild := func(parent *os.Root, childName string) (*os.Root, error) {
		children++
		child, err := parent.OpenRoot(childName)
		if err != nil {
			return child, err
		}
		codeParentSyncOwnChild(t, child)
		held = append(held, parent, child)
		info, err := parent.Stat(".")
		if err != nil {
			t.Fatal("controlled parent identity read failed")
		}
		paths, identities = append(paths, parent.Name()), append(identities, info)
		if parts[1] == "state" && childName == "sessions" || parts[1] == "namespace" && childName == opts.SessionID {
			if parts[2] == "before-permission" {
				t.Fatal(intentional)
			}
			_ = denyCodeParentSync(t, parent)
			opened, openErr := child.Stat(".")
			named, namedErr := parent.Lstat(childName)
			if openErr != nil || namedErr != nil || !opened.IsDir() || storage.IsReparseInfo(opened) || !os.SameFile(opened, named) {
				t.Fatal("controlled permission interrupted child identity check")
			}
			if pe, ok := product.AsError(storage.SyncRoot(parent)); !ok || pe.Code != product.CodeStorageUnavailable || pe.Message != "directory sync failed" {
				t.Fatal("NEEDS_CONTEXT: controlled actual parent sync was not denied")
			}
			probes++
			t.Fatal(intentional)
		}
		return child, nil
	}
	openFinal := func(id string, roots *storage.ResourceRoots, inspected *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
		finals++
		return jsonl.OpenBound(id, roots, inspected, header, opt)
	}
	if parts[0] == "readonly" {
		opened, err := openAgentSessionWithOpen(t.Context(), opts, openChild, openFinal)
		if opened != nil {
			t.Cleanup(func() { _ = opened.Close(context.Background()) })
		}
		if err != nil || opened == nil || parts[1] != "agent" {
			t.Fatal("controlled readonly factory unexpectedly returned")
		}
		t.Fatal(intentional)
	}
	created, _ := createAgentSessionWithOpen(t.Context(), opts, openChild, openFinal)
	if created != nil {
		t.Cleanup(func() { _ = created.Close(context.Background()) })
	}
	t.Fatal("controlled create factory unexpectedly returned")
}
