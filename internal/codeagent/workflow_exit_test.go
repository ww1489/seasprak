package codeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestCodeRegistryRejectsEveryWorkflowKind(t *testing.T) {
	def := agent.AgentDefinition{Name: "triage", Version: "w1", Kind: "workflow"}
	for _, delegable := range []bool{false, true} {
		def.Delegable = delegable
		_, err := agent.NewAgentRegistry("main", []agent.AgentDefinition{def})
		requireSessionCode(t, err, product.CodeInvalidArgument)
	}
}

func TestCodeBuiltinDelegationCannotRunLegacyWorkflow(t *testing.T) {
	var effects atomic.Int32
	main := testkit.NewFake(delegateCall("triage", `{"topic":"legacy"}`), testkit.Step{Text: "parent done"})
	ordinary := testkit.NewFake(testkit.Step{Text: "must not run"})
	opts := subagentOptions(agentRoots(t), "legacy-workflow-delegate", main,
		[]agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "Ordinary worker.", Model: ordinary, Delegable: true, Tools: []string{"probe"}}},
		[]tools.Definition{countedTool("probe", &effects, nil)})
	s := openSubagentSession(t, opts, true)
	in := submitPrompt(t, s, "delegate to the exited workflow")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	view := s.rt.manager.View()
	call := delegateRecord(t, s)
	if call.Observation == nil || call.Observation.Status != "failed" || call.Observation.Content != "invalid tool arguments" || call.Claimed || call.Observation.Executed || call.Observation.SideEffect != "none" {
		t.Fatal("legacy workflow delegation was not rejected before claim")
	}
	delegates := s.rt.registry.Delegates(agent.MainAgentName)
	if len(delegates) != 1 || delegates[0].Name != "worker" || delegates[0].Kind != agent.AgentKindAgent {
		t.Fatal("exited workflow remained a builtin delegation target")
	}
	if ordinary.Calls() != 0 || effects.Load() != 0 || len(view.Invocations) != 0 || len(view.InvocationBudgets) != 0 || len(view.InvocationMessages) != 0 || len(view.Calls) != 1 || main.Calls() != 2 || view.Traces[in.TraceID].Usage.LogicalModelCalls != 2 || view.Traces[in.TraceID].Usage.ToolExecutions != 0 {
		t.Fatal("legacy workflow delegation ran a child, a tool, or a fallback")
	}
}

func TestCodeOpenRejectsLegacyWorkflowFactsWithoutChangingJournal(t *testing.T) {
	for _, kind := range []string{"workflow_node", "model_attempt", "frozen_execution"} {
		t.Run(kind, func(t *testing.T) {
			root := agentRoots(t)
			main := testkit.NewFake()
			var effects atomic.Int32
			opts := Options{Workspace: root + "/ws", StateRoot: root + "/state", SessionID: "legacy-fact", Profile: ProfileMemory, Model: main, Tools: []tools.Definition{countedTool("probe", &effects, nil)}, Principal: "local", GenerationFingerprint: "exit-v1"}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			backend, err := jsonl.Open(opts.SessionID, opts.StateRoot, storage.Header{}, jsonl.Options{OpenExisting: true})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := backend.Load(t.Context(), opts.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{"id": "legacy", "traceId": "old-trace", "invocationId": "old-invocation", "nodeId": "m", "kind": "model", "state": "accepted"}
			if kind == "model_attempt" {
				payload = map[string]any{"id": "legacy", "purpose": "workflow_node", "state": "started"}
			}
			if kind == "frozen_execution" {
				payload = map[string]any{"id": "legacy", "origin": "workflow_node"}
			}
			raw, _ := json.Marshal(payload)
			_, err = backend.Append(t.Context(), opts.SessionID, storage.ExpectedCommit{ExpectedPreviousSeq: loaded.LastSeq}, storage.Commit{RecordType: "commit", Version: 1, CommitID: agent.MustID(), CommitSeq: loaded.LastSeq + 1, ExpectedPreviousSeq: loaded.LastSeq, ControlRecords: []storage.Record{{Type: kind, Version: 1, ID: "legacy", Payload: raw}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.Close(); err != nil {
				t.Fatal(err)
			}
			journal := journalPath(opts.StateRoot, opts.SessionID)
			lock := filepath.Join(filepath.Dir(journal), "writer.lock")
			if err := os.Remove(lock); err != nil {
				t.Fatal(err)
			}
			modes := map[string]os.FileMode{}
			if goruntime.GOOS == "windows" {
				t.Log("Unix permission assertions are not applicable on Windows; lock and bytes are checked")
			} else {
				for path, mode := range map[string]os.FileMode{filepath.Dir(filepath.Dir(journal)): 0o750, filepath.Dir(journal): 0o750, journal: 0o640} {
					if err := os.Chmod(path, mode); err != nil {
						t.Fatal(err)
					}
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != mode {
						t.Fatalf("fixture mode not established: %s %v", path, err)
					}
					modes[path] = mode
				}
			}
			before, err := os.ReadFile(journal)
			if err != nil {
				t.Fatal(err)
			}
			for _, readonly := range []bool{true, false} {
				t.Run(map[bool]string{true: "readonly", false: "writable"}[readonly], func(t *testing.T) {
					opts.ReadOnly = readonly
					opened, err := OpenAgentSession(t.Context(), opts)
					if opened != nil {
						_ = opened.Close(context.Background())
					}
					requireSessionCode(t, err, product.CodeIncompatibleVersion)
					after, err := os.ReadFile(journal)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(before, after) || main.Calls() != 0 || effects.Load() != 0 {
						t.Error("legacy rejection changed bytes or invoked model/tool")
					}
					if _, err := os.Lstat(lock); !os.IsNotExist(err) {
						t.Errorf("legacy rejection created writer.lock: %v", err)
					}
					for path, mode := range modes {
						info, err := os.Stat(path)
						if err != nil {
							t.Fatal(err)
						}
						if info.Mode().Perm() != mode {
							t.Errorf("legacy rejection changed mode: %s got %o want %o", path, info.Mode().Perm(), mode)
						}
						t.Logf("observed mode %o on %s", info.Mode().Perm(), path)
					}
				})
			}
		})
	}
}

func TestCodeLegacyTargetWithoutNodesNeverFallsBack(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "paused"}[paused], func(t *testing.T) {
			root := agentRoots(t)
			main := testkit.NewFake()
			opts := Options{Workspace: root + "/ws", StateRoot: root + "/state", SessionID: "legacy-target", Profile: ProfileMemory, Model: main, Principal: "local", GenerationFingerprint: "exit-v1"}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			generation := s.rt.generation
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			backend, err := jsonl.Open(opts.SessionID, opts.StateRoot, storage.Header{}, jsonl.Options{OpenExisting: true})
			if err != nil {
				t.Fatal(err)
			}
			manager, err := state.NewManager(backend, opts.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			cmd := agent.InputCommand{Kind: "prompt", TargetAgent: "triage", Content: json.RawMessage(`{"input":{}}`), Principal: "local", IdempotencyKey: "old-input"}
			target := agent.TargetAgent{Name: "triage", Version: "w1", Hash: "original-workflow-hash", Generation: generation}
			receipt, err := manager.Accept(t.Context(), cmd, target)
			if err != nil {
				t.Fatal(err)
			}
			if paused {
				if err := manager.SetTraceState(t.Context(), receipt.TraceID, "running", false); err != nil {
					t.Fatal(err)
				}
				if err := manager.SetTraceState(t.Context(), receipt.TraceID, "paused", false); err != nil {
					t.Fatal(err)
				}
			} else if err := manager.HoldIndependent(t.Context(), receipt.TraceID); err != nil {
				t.Fatal(err)
			}
			if err := backend.Close(); err != nil {
				t.Fatal(err)
			}
			beforeBytes, _ := os.ReadFile(journalPath(opts.StateRoot, opts.SessionID))
			opts.ReadOnly = true
			s, err = OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Snapshot(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			afterBytes, _ := os.ReadFile(journalPath(opts.StateRoot, opts.SessionID))
			if !bytes.Equal(beforeBytes, afterBytes) {
				t.Fatal("read-only history wrote legacy target")
			}
			opts.ReadOnly = false
			s, err = OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(t.Context())
			before := s.rt.manager.View()
			if paused {
				_, err = s.Resume(t.Context(), ResumeCommand{TraceID: receipt.TraceID, ExpectedRevision: before.LastSeq})
				requireSessionCode(t, err, product.CodeIncompatibleResume)
			} else {
				requireSessionCode(t, s.ContinueQueue(t.Context(), receipt.TraceID), product.CodeIncompatibleResume)
				_, err = s.ContinueQueued(t.Context(), ContinueQueueRequest{TraceIDs: []string{receipt.TraceID}, IdempotencyKey: "continue"})
				requireSessionCode(t, err, product.CodeIncompatibleResume)
				if !reflect.DeepEqual(before, s.rt.manager.View()) {
					t.Fatal("continue changed the legacy held target")
				}
				// Deliberately release only the test fixture's hold so schedule
				// reaches its exact target gate rather than skipping held work.
				if err := s.rt.do(t.Context(), func(rt *runtime) error {
					if err := rt.manager.ReleaseHold(t.Context(), receipt.TraceID); err != nil {
						return err
					}
					before = rt.manager.View()
					rt.schedule()
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			duplicate, err := s.SubmitInput(t.Context(), cmd)
			if err != nil || duplicate != receipt {
				t.Fatalf("original receipt lost: %+v %v", duplicate, err)
			}
			_, err = s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", TargetAgent: "triage", Content: cmd.Content})
			requireSessionCode(t, err, product.CodeUnsupportedCapability)
			after := s.rt.manager.View()
			if !reflect.DeepEqual(before, after) || main.Calls() != 0 || len(after.Invocations) != 0 || len(after.Calls) != 0 || after.Traces[receipt.TraceID].Target != target {
				t.Fatal("legacy target mutated or fell back")
			}
		})
	}
}
