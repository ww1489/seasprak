package sessions

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type selectionResumeModel struct {
	*testkit.FakeModel
	name string
}

func (m selectionResumeModel) Configuration() llm.ModelConfig {
	return llm.ModelConfig{Model: m.name, Version: m.name + "-v1"}
}

func TestApprovalResumeUsesCommittedModelSelection(t *testing.T) {
	for _, binding := range []string{"trace", "turn"} {
		t.Run(binding, func(t *testing.T) {
			id := agent.MustID()
			backend, err := memory.Open(id, store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			manager, err := state.NewManager(backend, id)
			if err != nil {
				t.Fatal(err)
			}
			initial := selectionResumeModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "gate", Name: "gate", Arguments: `{}`}}}), "initial"}
			a := selectionResumeModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "approved", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "A done"}), "A"}
			b := selectionResumeModel{testkit.NewFake(testkit.Step{Text: "B done"}), "B"}
			entered, gate := make(chan struct{}), make(chan struct{})
			runs := &atomic.Int32{}
			opts := Options{SessionID: id, Workspace: t.TempDir(), Principal: "user", Profile: ProfileMemory, GenerationFingerprint: "selection-resume-v1", Store: backend, Model: initial, Tools: []tools.Definition{
				{Name: "gate", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
					close(entered)
					select {
					case <-gate:
						return "ready", nil
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}},
				{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "approved", nil }},
			}}
			if _, err := alignTools(&opts); err != nil {
				t.Fatal(err)
			}
			s, err := Start(opts, manager, "gen")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			if binding == "trace" {
				if _, err := s.SetDefaultModel(t.Context(), SetDefaultModelRequest{Model: ModelChoice{Model: a}, ExpectedRevision: manager.View().LastSeq}); err != nil {
					t.Fatal(err)
				}
			}
			input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"approval"}`)})
			if err != nil {
				t.Fatal(err)
			}
			if binding == "turn" {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("gate not reached")
				}
				if _, err := s.SelectNextTurnModel(t.Context(), SelectNextTurnModelRequest{TraceID: input.TraceID, Model: ModelChoice{Model: a}, ExpectedRevision: manager.View().LastSeq}); err != nil {
					t.Fatal(err)
				}
				close(gate)
			}
			waitResumeCondition(t, func() bool {
				return manager.View().Traces[input.TraceID].State == "paused" || terminal(manager.View().Traces[input.TraceID].State)
			})
			view := manager.View()
			cp := view.Checkpoints[view.Traces[input.TraceID].CheckpointID]
			if view.Traces[input.TraceID].State != "paused" || cp.ModelConfigVersion != "A-v1" || a.Calls() != 1 || runs.Load() != 0 {
				t.Fatalf("unexpected approval checkpoint: %+v", cp)
			}
			if _, err := s.SetDefaultModel(t.Context(), SetDefaultModelRequest{Model: ModelChoice{Model: b}, ExpectedRevision: view.LastSeq}); err != nil {
				t.Fatal(err)
			}
			for interaction := range manager.View().Interactions {
				if _, err := s.RespondInteraction(t.Context(), InteractionResponse{InteractionID: interaction, Decision: "allowed-once", ExpectedRevision: manager.View().LastSeq}); err != nil {
					t.Fatal(err)
				}
			}
			receipt, err := s.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: manager.View().LastSeq})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(manager.View().Traces[input.TraceID].State) })
			op, err := manager.GetOperation(receipt.OperationID)
			if err != nil || op.State != "completed" || a.Calls() != 2 || b.Calls() != 0 || runs.Load() != 1 {
				t.Fatalf("resume=%+v err=%v A=%d B=%d tools=%d", op, err, a.Calls(), b.Calls(), runs.Load())
			}
			next, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"independent"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(manager.View().Traces[next.TraceID].State) })
			wantInitial := 0
			if binding == "turn" {
				wantInitial = 1
			}
			if manager.View().Traces[next.TraceID].State != "completed" || initial.Calls() != wantInitial || a.Calls() != 2 || b.Calls() != 1 || runs.Load() != 1 {
				t.Fatalf("calls initial=%d A=%d B=%d tools=%d", initial.Calls(), a.Calls(), b.Calls(), runs.Load())
			}
		})
	}
}

type selectionProgressStore struct {
	store.Store
	store.CheckpointBlobs
	change func(*store.StoredSession)
}

func (s selectionProgressStore) Load(ctx context.Context, id string) (store.StoredSession, error) {
	stored, err := s.Store.Load(ctx, id)
	if err == nil {
		s.change(&stored)
	}
	return stored, err
}

func TestApprovalResumeDefaultExceptionRejectsUnrelatedProgress(t *testing.T) {
	for _, mutation := range []string{"unknown-record", "history", "trace-scope", "generation", "next-turn", "tools", "activation", "operation", "revision"} {
		t.Run(mutation, func(t *testing.T) {
			f := waitingApprovalSession(t, nil)
			selected := testkit.NewFake(testkit.Step{Text: "B"})
			receipt, err := f.s.SetDefaultModel(t.Context(), SetDefaultModelRequest{Model: ModelChoice{Name: "B", Version: "1", Model: selected}, ExpectedRevision: f.manager.View().LastSeq})
			if err != nil {
				t.Fatal(err)
			}
			answerApproval(t, f, "allowed-once")
			if err := f.s.rt.do(t.Context(), func(rt *runtime) error {
				rt.opts.Store = selectionProgressStore{Store: rt.opts.Store, CheckpointBlobs: rt.opts.Store.(store.CheckpointBlobs), change: func(stored *store.StoredSession) {
					for i := range stored.Commits {
						commit := &stored.Commits[i]
						if commit.CommitSeq != receipt.AcceptedCommit {
							continue
						}
						if mutation == "unknown-record" {
							commit.ControlRecords = append(commit.ControlRecords, store.Record{Type: "unknown-control", ID: "unknown", Payload: json.RawMessage(`{}`)})
							return
						}
						if mutation == "history" {
							commit.Entries = append(commit.Entries, store.Record{Type: "message", ID: "new-history", Payload: json.RawMessage(`{}`)})
							return
						}
						for j := range commit.ControlRecords {
							rec := &commit.ControlRecords[j]
							if rec.Type != "selection" {
								continue
							}
							var choice state.Selection
							if err := json.Unmarshal(rec.Payload, &choice); err != nil {
								panic(err)
							}
							switch mutation {
							case "trace-scope":
								choice.Scope.TraceID = f.input.TraceID
							case "generation":
								choice.Scope.Generation = "other"
							case "next-turn":
								choice.ApplyAt = "next_turn"
							case "tools":
								choice.Kind = "tools"
							case "activation":
								choice.State = "active"
							case "operation":
								choice.OperationID = "other-operation"
							case "revision":
								choice.Revision++
							}
							rec.Payload, _ = json.Marshal(choice)
							return
						}
					}
				}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := f.manager.View()
			_, err = f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
			requireSessionCode(t, err, product.CodeIncompatibleResume)
			if !reflect.DeepEqual(before, f.manager.View()) || f.model.Calls() != 1 || selected.Calls() != 0 || f.runs.Load() != 0 {
				t.Fatal("invalid progress changed or executed checkpoint")
			}
		})
	}
}

func TestApprovalResumeRejectsPendingCurrentTraceSelection(t *testing.T) {
	for _, kind := range []string{"model", "tools"} {
		t.Run(kind, func(t *testing.T) {
			f := waitingApprovalSession(t, nil)
			selected := testkit.NewFake(testkit.Step{Text: "unapproved switch"})
			var err error
			if kind == "model" {
				_, err = f.s.SelectNextTurnModel(t.Context(), SelectNextTurnModelRequest{TraceID: f.input.TraceID, Model: ModelChoice{Name: "new", Version: "1", Model: selected}, ExpectedRevision: f.manager.View().LastSeq})
			} else {
				_, err = f.s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: f.input.TraceID, ToolNames: []string{}, ExpectedRevision: f.manager.View().LastSeq})
			}
			if err != nil {
				t.Fatal(err)
			}
			answerApproval(t, f, "allowed-once")
			before := f.manager.View()
			_, err = f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
			requireSessionCode(t, err, product.CodeIncompatibleResume)
			if !reflect.DeepEqual(before, f.manager.View()) || f.model.Calls() != 1 || selected.Calls() != 0 || f.runs.Load() != 0 {
				t.Fatal("rejected selection changed checkpoint execution")
			}
		})
	}
}
