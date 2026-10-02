package workflowagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type faultStore struct {
	storage.Store
	reject func(storage.Commit) error
}

func (f *faultStore) Append(ctx context.Context, id string, e storage.ExpectedCommit, c storage.Commit) (storage.CommitReceipt, error) {
	if f.reject != nil {
		if err := f.reject(c); err != nil {
			return storage.CommitReceipt{}, err
		}
	}
	return f.Store.Append(ctx, id, e, c)
}
func (*faultStore) Close() error { return nil }
func injectStore(t *testing.T, opts *WorkflowOptions) *faultStore {
	t.Helper()
	c, _, binding, err := compileOptions(*opts)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(workspaceBinding{ResourceType: storage.ResourceWorkflow, FormatVersion: 1, HostRealRoot: opts.Workspace, DefinitionHash: c.Hash, BindingVersion: binding})
	mem, err := memory.Open(opts.RunID, storage.Header{ResourceType: storage.ResourceWorkflow, RunID: opts.RunID, Workspace: raw})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mem.Close() })
	f := &faultStore{Store: mem}
	opts.Store = f
	return f
}
func waitExited(t *testing.T, w *WorkflowAgent) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		w.mu.Lock()
		active := w.active
		w.mu.Unlock()
		if active == nil {
			return
		}
		select {
		case <-active.done:
			return
		case <-time.After(time.Millisecond):
		}
	}
	t.Fatal("segment did not actually exit")
}

func TestWorkflowCommitFailuresNeverAdvanceOrRefund(t *testing.T) {
	for _, kind := range []string{"workflow_budget", "workflow_model_attempt", "workflow_tool_intent", "workflow_tool_observation", "node_result"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			m := testkit.NewFake(testkit.Step{Text: "accepted"})
			opts := testOptions(t, modelThenTool(), m, &calls)
			f := injectStore(t, &opts)
			w := newWorkflow(t, opts)
			f.reject = func(c storage.Commit) error {
				for _, r := range c.ControlRecords {
					if r.Type == kind || kind == "node_result" && r.Type == "workflow_node" {
						if kind == "node_result" {
							var p nodeRecord
							json.Unmarshal(r.Payload, &p)
							if p.Node.Kind != "tool" || p.Node.State != "completed" {
								continue
							}
						}
						return product.NewError(product.CodeStorageUnavailable, "synthetic commit failure")
					}
				}
				return nil
			}
			submit(t, w)
			waitExited(t, w)
			s, _ := w.Snapshot(t.Context())
			w.mu.Lock()
			broken := w.broken
			records := w.state.clone()
			w.mu.Unlock()
			requireCode(t, broken, product.CodeStorageUnavailable)
			if s.State == "completed" || s.ExecutionStopped {
				t.Fatalf("uncommitted terminal published %+v", s)
			}
			switch kind {
			case "workflow_budget", "workflow_model_attempt":
				if m.Calls() != 0 || calls.Load() != 0 || s.Usage.TransportRequests != 0 {
					t.Fatalf("request before registration/persist model=%d tool=%d usage=%+v", m.Calls(), calls.Load(), s.Usage)
				}
			case "workflow_tool_intent":
				if calls.Load() != 0 || s.Usage.ToolExecutions != 0 {
					t.Fatalf("tool before claim count=%d usage=%+v", calls.Load(), s.Usage)
				}
			default:
				if calls.Load() != 1 || s.Usage.ToolExecutions != 1 {
					t.Fatalf("lost committed occupancy count=%d usage=%+v", calls.Load(), s.Usage)
				}
			}
			for _, n := range records.Nodes {
				if n.Kind == "tool" && n.State == "completed" {
					t.Error("failed tool result commit advanced node")
				}
			}
			f.reject = nil
			if err := w.Close(context.Background()); err == nil {
				t.Error("close concealed commit failure")
			}
			reopened, err := OpenWorkflowAgent(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(context.Background())
			after, _ := reopened.Snapshot(t.Context())
			if after.Usage != s.Usage {
				t.Fatalf("occupancy changed after reopen before=%+v after=%+v", s.Usage, after.Usage)
			}
			_, err = reopened.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
			requireCode(t, err, product.CodeIncompatibleResume)
		})
	}
}

func TestWorkflowUnknownEffectIsRetainedAndCannotResume(t *testing.T) {
	var calls atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &calls)
	opts.Tools[0].Run = func(context.Context, json.RawMessage) (string, error) {
		calls.Add(1)
		return "", product.NewError(product.CodeResourceUnavailable, "synthetic backend fault")
	}
	w := newWorkflow(t, opts)
	submit(t, w)
	s := waitStopped(t, w)
	if s.State != "failed" || s.ErrorCode != product.CodeReconciliationRequired || calls.Load() != 1 || s.Usage.ToolExecutions != 1 {
		t.Fatalf("unknown %+v calls=%d", s, calls.Load())
	}
	w.mu.Lock()
	unknown := w.state.hasUnknown()
	w.mu.Unlock()
	if !unknown {
		t.Fatal("unknown rewritten as no-effect")
	}
	_, err := w.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
	if err == nil {
		t.Fatal("unknown replay allowed")
	}
	if calls.Load() != 1 {
		t.Fatal("unknown executed twice")
	}
}

func TestWorkflowReplayRejectsForgedCombinationFacts(t *testing.T) {
	var count atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &count)
	f := injectStore(t, &opts)
	w := newWorkflow(t, opts)
	submit(t, w)
	s := waitStopped(t, w)
	loaded, err := f.Load(t.Context(), opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"unknown_record", "identity", "budget_refund", "claim_removed", "accepted_attempt_missing"} {
		t.Run(mutation, func(t *testing.T) {
			forged := storage.CloneSession(loaded)
			switch mutation {
			case "unknown_record":
				forged.Commits[0].ControlRecords[0].Type = "unknown_state"
			case "identity":
				var p initialRecord
				json.Unmarshal(forged.Commits[0].ControlRecords[0].Payload, &p)
				p.RunID = "another"
				forged.Commits[0].ControlRecords[0] = record("workflow_initialized", "initial", p)
			case "budget_refund":
				for i := range forged.Commits {
					for j, r := range forged.Commits[i].ControlRecords {
						if r.Type == "workflow_tool_intent" {
							var p toolIntent
							json.Unmarshal(r.Payload, &p)
							p.Budget.Aggregate.ToolExecutions = 0
							forged.Commits[i].ControlRecords[j] = record(r.Type, r.ID, p)
						}
					}
				}
			case "claim_removed":
				for i := range forged.Commits {
					var kept []storage.Record
					for _, r := range forged.Commits[i].ControlRecords {
						if r.Type != "workflow_tool_intent" {
							kept = append(kept, r)
						}
					}
					forged.Commits[i].ControlRecords = kept
				}
			case "accepted_attempt_missing":
				for i := range forged.Commits {
					for j, r := range forged.Commits[i].ControlRecords {
						if r.Type == "workflow_node" {
							var p nodeRecord
							json.Unmarshal(r.Payload, &p)
							if p.Node.State == "completed" {
								p.Node.Kind = "model"
								forged.Commits[i].ControlRecords[j] = record(r.Type, r.ID, p)
							}
						}
					}
				}
			}
			_, err := replay(forged, opts.RunID)
			requireCode(t, err, product.CodeIncompatibleVersion)
		})
	}
	if count.Load() != 1 || s.Usage.ToolExecutions != 1 {
		t.Fatal("replay ran effects")
	}
}
