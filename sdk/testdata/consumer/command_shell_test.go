package consumer_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ww1489/seasprak/sdk"
)

func TestConsumerExecuteCommandIsUserShell(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	model := &fakeModel{}
	store := &memStore{}
	s, err := sdk.CreateAgentSession(t.Context(), sdk.SessionOptions{
		Workspace: workspace, StateRoot: "memory", Profile: sdk.ProfileMemory,
		SessionID: "consumer-user-shell", Model: model, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	request := sdk.CommandRequest{Command: "echo user-command >> calls.txt", Cwd: outside, Timeout: 10 * time.Second}
	for i := 0; i < 2; i++ {
		var result sdk.CommandResult
		result, err = s.ExecuteCommand(t.Context(), request)
		if err != nil || !result.Started || !result.Terminated || result.ExitCode != 0 || result.Cancelled || result.TimedOut {
			t.Fatalf("explicit execution %d: result=%+v err=%v", i, result, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(outside, "calls.txt"))
	if err != nil || strings.Count(string(data), "user-command") != 2 {
		t.Fatalf("explicit executions must each run once: bytes=%d err=%v", len(data), err)
	}
	model.mu.Lock()
	calls := model.calls
	model.mu.Unlock()
	if calls != 0 {
		t.Fatalf("user command invoked model %d times", calls)
	}
	if !store.used() {
		t.Fatal("command history was not saved")
	}
}
