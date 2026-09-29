package consumer_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/sdk"
)

func assertConsumerApprovalBlocksShell(t *testing.T, s *sdk.AgentSession, workspace string) {
	t.Helper()
	before, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.ExecuteCommand(t.Context(), sdk.CommandRequest{Command: "echo run >approval-shell-runs.txt"})
	pe, ok := sdk.AsError(err)
	if !ok || pe.Code != "state_conflict" || out.Started || out.Terminated {
		t.Fatalf("approval admitted a host shell: %+v %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "approval-shell-runs.txt")); !os.IsNotExist(err) {
		t.Fatal("blocked shell produced a side effect", err)
	}
	after, err := s.Snapshot(t.Context())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("blocked shell changed public history, revision or approval state", err)
	}
}
