package codeagent

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/testkit"
)

func startHostCommandApprovalDisk(t *testing.T, hook func(context.Context, agent.FrozenExecution) error) (approvalSessionFixture, Options) {
	t.Helper()
	model := versionedPauseModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "finished"})}
	runs := &atomic.Int32{}
	def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, BeforeCall: []func(context.Context, agent.FrozenExecution) error{hook}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "approved result", nil }}
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Principal: "host-user", Profile: ProfileMemory, GenerationFingerprint: "command-approval-disk-v1", Model: model, Tools: []tools.Definition{def}}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.clock = newManualActivityClock(); return nil }); err != nil {
		t.Fatal(err)
	}
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return approvalSessionFixture{s: s, manager: s.rt.manager, model: model, input: in, runs: runs}, opts
}

type commandFileImage struct {
	Content string
	Mode    fs.FileMode
	ModTime time.Time
}

func commandDiskImage(t *testing.T, root string) map[string]commandFileImage {
	t.Helper()
	out := make(map[string]commandFileImage)
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		image := commandFileImage{Mode: info.Mode(), ModTime: info.ModTime()}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			image.Content = string(data)
		}
		out[path] = image
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
