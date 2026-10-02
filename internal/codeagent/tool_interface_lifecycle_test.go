package codeagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

// Match the same durable boundaries used by the crash/recovery fixture, while
// rejecting the append instead of crashing the test process.
type syncInterfaceFaultStore struct {
	store.Store
	stage    string
	rejected chan store.Commit
}

func (*syncInterfaceFaultStore) Close() error { return nil }
func (s *syncInterfaceFaultStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	match := crashWindowOf(commit) == s.stage
	for _, record := range commit.ControlRecords {
		if s.stage == "frozen_execution" && record.Type == s.stage {
			match = true
		}
	}
	if match {
		s.rejected <- commit
		return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "injected synchronous tool append failure")
	}
	return s.Store.Append(ctx, id, expected, commit)
}

func TestToolInterfaceSessionSaveFailures(t *testing.T) {
	for _, kind := range []string{"invokable", "enhanced-invokable"} {
		for _, stage := range []string{"frozen_execution", windowIntent, windowObservation} {
			t.Run(kind+"/"+stage, func(t *testing.T) {
				id := agent.MustID()
				backend, err := memory.Open(id, store.Header{})
				if err != nil {
					t.Fatal(err)
				}
				defer backend.Close()
				faults := &syncInterfaceFaultStore{Store: backend, stage: stage, rejected: make(chan store.Commit, 1)}
				gate := make(chan struct{})
				var runs atomic.Int32
				model := testkit.NewFake(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "must not continue"})
				def := tools.Definition{Name: "work", Version: "1", ToolInterface: kind, Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) {
					runs.Add(1)
					return "actual result", nil
				}}
				s, err := CreateAgentSession(t.Context(), Options{SessionID: id, Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Store: faults, Model: model, Tools: []tools.Definition{def}, ResourceScheduler: tools.NewResourceScheduler()})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close(context.Background())
				receipt := submitOutput(t, s)
				frame := activityFrame(t, s)
				close(gate)
				var rejected store.Commit
				select {
				case rejected = <-faults.rejected:
				case <-time.After(10 * time.Second):
					t.Fatal("append boundary not reached")
				}
				select {
				case <-frame.done:
				case <-time.After(10 * time.Second):
					t.Fatal("failed segment did not exit")
				}
				requireSessionCode(t, s.rt.manager.Fault(), product.CodeStorageUnavailable)
				if stage == windowObservation {
					observations := 0
					for _, record := range rejected.ControlRecords {
						if record.Type != "tool_call" {
							continue
						}
						var call agent.ToolRecord
						if err := json.Unmarshal(record.Payload, &call); err != nil {
							t.Fatal(err)
						}
						if call.Observation == nil || !call.Observation.Executed || call.Observation.Status != "succeeded" || call.Observation.Content != "actual result" {
							t.Fatalf("rejected observation lost actual backend result: %+v", call)
						}
						observations++
					}
					if observations != 1 {
						t.Fatalf("rejected observations=%d", observations)
					}
				}
				wantRuns := int32(0)
				if stage == windowObservation {
					wantRuns = 1
				}
				view := s.rt.manager.View()
				if runs.Load() != wantRuns || model.Calls() != 1 || view.LastSeq != rejected.ExpectedPreviousSeq {
					t.Fatalf("runs=%d model=%d revision=%d rejected=%d", runs.Load(), model.Calls(), view.LastSeq, rejected.ExpectedPreviousSeq)
				}
				// Reconstruct the committed journal, not only the live manager view.
				replayed, err := state.NewManager(backend, id)
				if err != nil {
					t.Fatal(err)
				}
				for _, v := range []state.View{view, replayed.View()} {
					wantFrozen := 1
					if stage == "frozen_execution" {
						wantFrozen = 0
					}
					if len(v.Calls) != 1 || len(v.FrozenExecutions) != wantFrozen || v.Traces[receipt.TraceID].Usage.ToolExecutions != int(wantRuns) {
						t.Fatalf("calls=%d frozen=%d trace=%+v", len(v.Calls), len(v.FrozenExecutions), v.Traces[receipt.TraceID])
					}
					for _, call := range v.Calls {
						if call.Claimed != (stage == windowObservation) || call.Observation != nil {
							t.Fatalf("failed append changed persisted call: %+v", call)
						}
					}
				}
			})
		}
	}
}

func TestToolInterfaceSessionCancelBeforeClaim(t *testing.T) {
	for _, kind := range []string{"invokable", "enhanced-invokable"} {
		t.Run(kind, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var runs atomic.Int32
			def := tools.Definition{Name: "work", Version: "1", ToolInterface: kind, Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "read"}, BeforeCall: []func(context.Context, agent.FrozenExecution) error{func(context.Context, agent.FrozenExecution) error {
				close(entered)
				<-release
				return nil
			}}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "unexpected", nil }}
			model := outputModel()
			s, err := CreateAgentSession(t.Context(), Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{def}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			receipt := submitOutput(t, s)
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("pre-claim hook not entered")
			}
			frame := activityFrame(t, s)
			cancelled := make(chan error, 1)
			go func() { cancelled <- s.Cancel(context.Background(), receipt.TraceID) }()
			select {
			case <-frame.ctx.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("cancel intent not accepted")
			}
			view := s.rt.manager.View()
			trace := view.Traces[receipt.TraceID]
			if trace.State != "cancelling" || trace.ExecutionStopped || trace.Settled || len(view.FrozenExecutions) != 1 || len(view.Calls) != 1 || runs.Load() != 0 || trace.Usage.ToolExecutions != 0 {
				t.Fatalf("premature stop or claim: trace=%+v runs=%d", trace, runs.Load())
			}
			select {
			case err := <-cancelled:
				t.Fatalf("cancel returned before hook exit: %v", err)
			default:
			}
			close(release)
			select {
			case err := <-cancelled:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("cancel did not complete")
			}
			view = s.rt.manager.View()
			trace = view.Traces[receipt.TraceID]
			if trace.State != "cancelled" || !trace.ExecutionStopped || !trace.Settled || trace.Usage.ToolExecutions != 0 || runs.Load() != 0 || model.Calls() != 1 {
				t.Fatalf("unsafe cancellation: trace=%+v runs=%d model=%d", trace, runs.Load(), model.Calls())
			}
			for _, call := range view.Calls {
				if call.Claimed || call.Observation == nil || call.Observation.Status != "cancelled" || call.Observation.Executed || call.Observation.SideEffect != "none" {
					t.Fatalf("pre-claim cancellation=%+v", call)
				}
			}
		})
	}
}
