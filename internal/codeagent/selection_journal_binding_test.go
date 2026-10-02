package codeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2SelectionDiskPendingContentBoundToAcceptance(t *testing.T) {
	for _, mutation := range []string{"unchanged", "explicit-invocation", "names", "versions", "model"} {
		t.Run(mutation, func(t *testing.T) {
			original := selectionConfiguredModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "approved", Name: "work", Arguments: `{}`}}}), selectionTrustedConfig("A")}
			var runs atomic.Int32
			opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Principal: "operator", Model: original, GenerationFingerprint: "selection-binding-v1", Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "done", nil }}}}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"approval"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return s.rt.manager.View().Traces[input.TraceID].State == "paused" })
			var receipt state.OperationReceipt
			if mutation == "model" {
				receipt, err = s.SelectNextTurnModel(t.Context(), SelectNextTurnModelRequest{TraceID: input.TraceID, Model: ModelChoice{Model: original}, ExpectedRevision: s.rt.manager.View().LastSeq, IdempotencyKey: "pending"})
			} else {
				invocation := ""
				if mutation == "explicit-invocation" {
					invocation = s.rt.manager.View().Traces[input.TraceID].InvocationID
				}
				receipt, err = s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: input.TraceID, InvocationID: invocation, ToolNames: []string{"work"}, ExpectedRevision: s.rt.manager.View().LastSeq, IdempotencyKey: "pending"})
			}
			if err != nil {
				t.Fatal(err)
			}
			digest := s.rt.manager.View().Operations[receipt.OperationID].Digest
			if err = s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			path := journalPath(opts.StateRoot, opts.SessionID)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
			changed := false
			for i := 1; i < len(lines); i++ {
				var commit store.Commit
				if err = json.Unmarshal(lines[i], &commit); err != nil {
					t.Fatal(err)
				}
				if commit.CommitSeq != receipt.AcceptedCommit {
					continue
				}
				for j := range commit.ControlRecords {
					record := &commit.ControlRecords[j]
					if record.Type != "selection" {
						continue
					}
					var choice state.Selection
					if err = json.Unmarshal(record.Payload, &choice); err != nil {
						t.Fatal(err)
					}
					switch mutation {
					case "names":
						choice.ToolNames = nil
						choice.ToolVersions = []string{}
					case "versions":
						choice.ToolVersions = []string{"forged-version"}
					case "model":
						choice.ModelName = "forged-model"
					}
					record.Payload, err = json.Marshal(choice)
					if err != nil {
						t.Fatal(err)
					}
					changed = true
				}
				lines[i], err = json.Marshal(commit)
				if err != nil {
					t.Fatal(err)
				}
			}
			if !changed {
				t.Fatal("pending journal record missing")
			}
			if err = os.WriteFile(path, append(bytes.Join(lines, []byte("\n")), '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			rebuilt := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "resumed"}), selectionTrustedConfig("A")}
			opts.Model = rebuilt
			opened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = opened.Close(context.Background()) })
			if opened.rt.manager == s.rt.manager || opened.rt.opts.Store == s.rt.opts.Store || opened.rt.manager.View().Operations[receipt.OperationID].Digest != digest {
				t.Fatal("not a fresh replay with original acceptance digest")
			}
			if snap := approvalSnapshot(t, opened); len(snap.Interactions) != 0 || len(snap.Approvals) != 0 {
				t.Fatal("Open created pending approval before Resume")
			}
			before := opened.rt.manager.View()
			_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: before.LastSeq})
			if mutation != "unchanged" && mutation != "explicit-invocation" {
				requireSessionCode(t, err, product.CodeIncompatibleResume)
				if !reflect.DeepEqual(before, opened.rt.manager.View()) || original.Calls() != 1 || rebuilt.Calls() != 0 || runs.Load() != 0 {
					t.Fatal("tampered pending selection executed or mutated state")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				waitResumeCondition(t, func() bool {
					tr := opened.rt.manager.View().Traces[input.TraceID]
					return tr.State == "paused" || terminal(tr.State)
				})
				waiting := approvalSnapshot(t, opened)
				if waiting.Traces[input.TraceID].State != "paused" || len(waiting.Interactions) != 1 || !reflect.DeepEqual(before.Calls, waiting.Calls) || !reflect.DeepEqual(before.Traces[input.TraceID].Usage, waiting.Traces[input.TraceID].Usage) || original.Calls() != 1 || rebuilt.Calls() != 0 || runs.Load() != 0 {
					t.Fatal("unanswered Resume lost pending call or executed work")
				}
				answerCommand(t, opened, "allowed-once")
				if _, err = opened.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: waiting.Revision}); err != nil {
					t.Fatal(err)
				}
				waitResumeCondition(t, func() bool { return terminal(opened.rt.manager.View().Traces[input.TraceID].State) })
				if opened.rt.manager.View().Traces[input.TraceID].State != "completed" || original.Calls() != 1 || rebuilt.Calls() != 1 || runs.Load() != 1 {
					t.Fatal("unchanged compatible journal did not resume exactly once")
				}
			}
		})
	}
}
