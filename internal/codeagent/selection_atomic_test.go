package codeagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type selectionTurnFaultStore struct {
	store.Store
	armed    atomic.Bool
	rejected atomic.Int32
}

func (s *selectionTurnFaultStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	if s.armed.Load() {
		for _, record := range commit.ControlRecords {
			if record.Type != "turn" {
				continue
			}
			var turn agent.TurnRecord
			if json.Unmarshal(record.Payload, &turn) == nil && !turn.Ended && len(turn.CallIDs) == 0 {
				s.rejected.Add(1)
				return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "injected new turn commit failure")
			}
		}
	}
	return s.Store.Append(ctx, id, expected, commit)
}

func TestP2SelectionActivationAndBeginTurnAreAtomic(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "failure"}[fail], func(t *testing.T) {
			id := agent.MustID()
			backend, err := memory.Open(id, store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			fault := &selectionTurnFaultStore{Store: backend}
			manager, err := state.NewManager(fault, id)
			if err != nil {
				t.Fatal(err)
			}
			gate, entered := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-gate:
				default:
					close(gate)
				}
			}()
			var runs atomic.Int32
			original := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "original", Name: "work", Arguments: `{}`}}})
			selected := testkit.NewFake(testkit.Step{Text: "selected"})
			opts := Options{SessionID: id, Workspace: t.TempDir(), Profile: ProfileMemory, Store: fault, Model: original, Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
				close(entered)
				select {
				case <-gate:
				case <-ctx.Done():
					return "", ctx.Err()
				}
				runs.Add(1)
				return "done", nil
			}}}}
			if _, err := alignTools(&opts); err != nil {
				t.Fatal(err)
			}
			s, err := Start(opts, manager, "gen")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool {
				select {
				case <-entered:
					return true
				default:
					return false
				}
			})
			firstTurn := manager.View().Traces[input.TraceID].Usage.ModelCallID
			if _, err := s.SelectNextTurnModel(t.Context(), SelectNextTurnModelRequest{TraceID: input.TraceID, Model: ModelChoice{Name: "selected", Version: "1", Model: selected}, ExpectedRevision: manager.View().LastSeq}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: input.TraceID, ToolNames: []string{}, ExpectedRevision: manager.View().LastSeq}); err != nil {
				t.Fatal(err)
			}
			fault.armed.Store(fail)
			close(gate)
			waitResumeCondition(t, func() bool { return manager.Fault() != nil || terminal(manager.View().Traces[input.TraceID].State) })
			view := manager.View()
			if fail {
				if fault.rejected.Load() != 1 || runs.Load() != 1 || original.Calls() != 1 || selected.Calls() != 0 || len(view.Turns) != 1 || view.Traces[input.TraceID].Usage.LogicalModelCalls != 1 {
					t.Fatalf("failure counters rejected=%d tools=%d A=%d B=%d turns=%d usage=%+v", fault.rejected.Load(), runs.Load(), original.Calls(), selected.Calls(), len(view.Turns), view.Traces[input.TraceID].Usage)
				}
				for _, choice := range view.Selections {
					if choice.State != "pending" {
						t.Fatalf("failed BeginTurn partially activated selection: %+v", choice)
					}
				}
			} else {
				if runs.Load() != 1 || original.Calls() != 1 || selected.Calls() != 1 || len(view.Turns) != 2 || view.Traces[input.TraceID].State != "completed" {
					t.Fatal("selected turn did not execute exactly once")
				}
				stored, err := backend.Load(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				activationCommits := 0
				for _, commit := range stored.Commits {
					activations, turns, budgets := 0, 0, 0
					for _, record := range commit.ControlRecords {
						switch record.Type {
						case "selection":
							var choice state.Selection
							_ = json.Unmarshal(record.Payload, &choice)
							if choice.State == "active" {
								activations++
							}
						case "turn":
							var turn agent.TurnRecord
							_ = json.Unmarshal(record.Payload, &turn)
							if turn.ID != firstTurn && turn.SelectionRevision == commit.ExpectedPreviousSeq {
								turns++
							}
						case "trace":
							var trace state.TraceState
							_ = json.Unmarshal(record.Payload, &trace)
							if trace.Usage.LogicalModelCalls == 2 {
								budgets++
							}
						}
					}
					if activations > 0 {
						activationCommits++
						if activations != 2 || turns != 1 || budgets != 1 {
							t.Fatalf("activation is not atomic: choices=%d turns=%d budgets=%d", activations, turns, budgets)
						}
					}
				}
				if activationCommits != 1 {
					t.Fatalf("activation commits=%d", activationCommits)
				}
			}
		})
	}
}
