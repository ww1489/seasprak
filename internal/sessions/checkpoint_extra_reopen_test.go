package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

// This fixture stays within the supported JSON tree + json.Number boundary.
// The independent Eino test documents RawMessage's named-slice limitation.
func checkpointReopenTree() map[string]any {
	return map[string]any{"tree": []any{"synthetic-private-reopen", true, nil, map[string]any{"leaf": float64(1.25)}}, "number": json.Number("9007199254740993")}
}

type checkpointReopenModel struct {
	inner     *testkit.FakeModel
	reader    bool
	inspected atomic.Bool
}

func (*checkpointReopenModel) Configuration() llm.ModelConfig {
	return llm.ModelConfig{Version: "checkpoint-extra-reopen-v1"}
}
func (m *checkpointReopenModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	if m.reader {
		sawAssistant, sawTool := false, false
		for _, msg := range in {
			if msg.Role == schema.AgenticRoleTypeAssistant {
				sawAssistant = true
				if len(msg.ContentBlocks) != 2 || msg.ResponseMeta == nil || msg.ContentBlocks[1].AssistantGenText == nil {
					return nil, fmt.Errorf("checkpoint assistant structure lost")
				}
				values := []any{msg.Extra["provider"], msg.ContentBlocks[0].Extra["provider"], msg.ResponseMeta.Extension, msg.ContentBlocks[1].AssistantGenText.Extension}
				for i, value := range values {
					if !reflect.DeepEqual(value, checkpointReopenTree()) {
						return nil, fmt.Errorf("checkpoint location %d concrete JSON tree or Number lost: %#v", i, value)
					}
				}
			}
			for _, block := range msg.ContentBlocks {
				if result := block.FunctionToolResult; result != nil {
					if sawTool || result.CallID != "checkpoint-work" || len(result.Content) != 1 || result.Content[0].Text == nil || result.Content[0].Text.Text != "checkpoint tool result" {
						return nil, fmt.Errorf("checkpoint tool result lost or duplicated")
					}
					sawTool = true
				}
			}
		}
		if !sawAssistant || !sawTool {
			return nil, fmt.Errorf("resume skipped checkpoint assistant or tool result")
		}
		m.inspected.Store(true)
	}
	msg, err := m.inner.Generate(ctx, in, opts...)
	if err != nil || m.reader {
		return msg, err
	}
	msg.Extra["provider"] = checkpointReopenTree()
	msg.ContentBlocks[0].Extra = map[string]any{"provider": checkpointReopenTree()}
	msg.ResponseMeta = &schema.AgenticResponseMeta{Extension: checkpointReopenTree()}
	msg.ContentBlocks = append(msg.ContentBlocks, schema.NewContentBlock(&schema.AssistantGenText{Text: "visible checkpoint text", Extension: checkpointReopenTree()}))
	return msg, nil
}
func (m *checkpointReopenModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

type checkpointReopenManifest struct {
	Trace       string
	WriterTools int32
	Revision    uint64
}

func TestCheckpointExtraIndependentProcessReopen(t *testing.T) {
	if role := os.Getenv("SEASPRAK_CHECKPOINT_TEST_ROLE"); role != "" {
		checkpointExtraProcess(t, role, os.Getenv("SEASPRAK_CHECKPOINT_TEST_ROOT"), os.Getenv("SEASPRAK_CHECKPOINT_TEST_POINT"))
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range []string{"after-model", "after-tool"} {
		t.Run(point, func(t *testing.T) {
			root := t.TempDir()
			for _, role := range []string{"writer", "reader"} {
				ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
				cmd := exec.CommandContext(ctx, executable, "-test.run=^TestCheckpointExtraIndependentProcessReopen$", "-test.count=1", "-test.v")
				cmd.Env = append(os.Environ(), "SEASPRAK_CHECKPOINT_TEST_ROLE="+role, "SEASPRAK_CHECKPOINT_TEST_ROOT="+root, "SEASPRAK_CHECKPOINT_TEST_POINT="+point)
				output, err := cmd.CombinedOutput()
				cancel()
				if err != nil {
					t.Fatalf("%s subprocess: %v\n%s", role, err, output)
				}
				t.Logf("%s subprocess exited successfully\n%s", role, output)
			}
		})
	}
}

func checkpointExtraProcess(t *testing.T, role, root, point string) {
	t.Helper()
	reader := role == "reader"
	if role != "writer" && !reader {
		t.Fatal("unknown subprocess role")
	}
	afterTool := point == "after-tool"
	workspace := filepath.Join(root, "workspace")
	if !reader {
		for _, dir := range []string{workspace, filepath.Join(root, "state")} {
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
		}
	}
	gate, entered := make(chan struct{}), make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(gate) }) }
	defer unblock()
	var runs atomic.Int32
	first := testkit.Step{Finish: "tool_calls", ToolCalls: []schema.FunctionToolCall{{CallID: "checkpoint-work", Name: "work", Arguments: `{}`}}}
	if !afterTool && !reader {
		first.Gate = gate
	}
	fake := testkit.NewFake(first)
	if reader {
		fake = testkit.NewFake(testkit.Step{Text: "resumed answer"})
	}
	m := &checkpointReopenModel{inner: fake, reader: reader}
	opts := Options{SessionID: "checkpoint-extra-process", Workspace: workspace, StateRoot: filepath.Join(root, "state"), Profile: ProfileMemory, Model: m, GenerationFingerprint: "checkpoint-extra-process-v1", Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		runs.Add(1)
		if !reader && afterTool {
			close(entered)
			select {
			case <-gate:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "checkpoint tool result", nil
	}}}}
	var s *AgentSession
	var err error
	if reader {
		s, err = OpenAgentSession(t.Context(), opts)
	} else {
		s, err = CreateAgentSession(t.Context(), opts)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		unblock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	manifestPath := filepath.Join(root, "manifest.json")
	if !reader {
		receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"pause with nested Extra"}`)})
		if err != nil {
			t.Fatal(err)
		}
		waitResumeCondition(t, func() bool {
			if !afterTool {
				return fake.Calls() == 1
			}
			select {
			case <-entered:
				return true
			default:
				return false
			}
		})
		done := make(chan error, 1)
		go func() { _, err := s.Pause(t.Context(), receipt.TraceID); done <- err }()
		waitResumeCondition(t, func() bool { return len(s.rt.manager.View().Operations) == 1 })
		if err := s.rt.do(t.Context(), func(*runtime) error { return nil }); err != nil {
			t.Fatal(err)
		}
		unblock()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Pause timed out")
		}
		snap, err := s.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		wantRuns := int32(0)
		if afterTool {
			wantRuns = 1
		}
		if snap.Traces[receipt.TraceID].State != "paused" || !snap.Resume[receipt.TraceID].CanResume || fake.Calls() != 1 || runs.Load() != wantRuns {
			t.Fatalf("writer state=%s resume=%v model=%d tools=%d", snap.Traces[receipt.TraceID].State, snap.Resume[receipt.TraceID].CanResume, fake.Calls(), runs.Load())
		}
		assertNoPrivateReplay(t, "writer snapshot", snap)
		data, err := json.Marshal(checkpointReopenManifest{Trace: receipt.TraceID, WriterTools: runs.Load(), Revision: snap.Revision})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifestPath, data, 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	// The reader process has never encoded a checkpoint or run the writer model.
	// Its only registration source is the production package initialization.
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest checkpointReopenManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	before := s.rt.manager.View()
	snap, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if fake.Calls() != 0 || runs.Load() != 0 || snap.Revision != manifest.Revision || !reflect.DeepEqual(before, s.rt.manager.View()) || snap.Traces[manifest.Trace].State != "paused" || !snap.Resume[manifest.Trace].CanResume {
		t.Fatal("Open/Snapshot ran work, changed facts, or lost checkpoint")
	}
	assertNoPrivateReplay(t, "reader snapshot", snap)
	// Durable product JSON retains its existing JSON semantics; it is distinct
	// from the runner gob state whose Number type is checked by the model above.
	replay, err := agent.ConvertToLLM(before.Messages)
	if err != nil {
		t.Fatal(err)
	}
	var jsonTree any
	treeJSON, err := json.Marshal(checkpointReopenTree())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(treeJSON, &jsonTree); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range replay {
		if msg.Role == schema.AgenticRoleTypeAssistant {
			found = true
			if !reflect.DeepEqual(msg.Extra["provider"], jsonTree) {
				t.Fatal("durable JSON replay changed existing JSON semantics")
			}
		}
	}
	if !found {
		t.Fatal("durable assistant missing")
	}
	if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: manifest.Trace, ExpectedRevision: snap.Revision}); err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[manifest.Trace].State) })
	final := s.rt.manager.View().Traces[manifest.Trace]
	if final.State != "completed" || fake.Calls() != 1 || !m.inspected.Load() || manifest.WriterTools+runs.Load() != 1 || final.Usage.ToolExecutions != 1 || final.Usage.LogicalModelCalls != 2 {
		t.Fatalf("reader state=%s error=%q model=%d inspected=%v tools=%d+%d usage=%+v", final.State, final.Error, fake.Calls(), m.inspected.Load(), manifest.WriterTools, runs.Load(), final.Usage)
	}
	if afterTool && runs.Load() != 0 || !afterTool && runs.Load() != 1 {
		t.Fatal("wrong tool execution phase")
	}
}
