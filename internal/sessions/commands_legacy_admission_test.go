package sessions

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

func TestP2CommandLegacyApprovalBlocksNewShell(t *testing.T) {
	opts, process, trace := legacyCommandFixture(t, false)
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	before := opened.rt.manager.View()
	snapshot := approvalSnapshot(t, opened)
	request := CommandRequest{Command: shellScript(t, "echo run >>runs.txt", "echo run>>runs.txt")}
	out, err := opened.ExecuteCommand(t.Context(), request)
	requireSessionCode(t, err, product.CodeStateConflict)
	if out.Started || out.Terminated {
		t.Fatal("legacy approval admitted new shell")
	}
	if _, err := os.Stat(filepath.Join(opts.Workspace, "runs.txt")); !os.IsNotExist(err) {
		t.Fatal("rejected shell caused side effect", err)
	}
	if !reflect.DeepEqual(before, opened.rt.manager.View()) || !reflect.DeepEqual(snapshot, approvalSnapshot(t, opened)) || process.calls.Load() != 0 {
		t.Fatal("rejected command changed legacy history, leaf, revision or execution")
	}
	if err := opened.Cancel(t.Context(), trace); err != nil {
		t.Fatal(err)
	}
	out, err = opened.ExecuteCommand(t.Context(), request)
	if err != nil || !out.Started || !out.Terminated || out.ExitCode != 0 || shellRuns(t, opts.Workspace) != 1 || process.calls.Load() != 0 {
		t.Fatal("cancel did not release shell admission or executed old tool", err)
	}
}
