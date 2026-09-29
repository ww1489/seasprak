package sessions

import (
	"context"
	"encoding/json"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type searchSelectionFaultStore struct {
	store.Store
	armed    atomic.Bool
	rejected atomic.Int32
}

func (s *searchSelectionFaultStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	if s.armed.Load() {
		for _, r := range commit.ControlRecords {
			if r.Type == "selection" {
				var choice state.Selection
				_ = json.Unmarshal(r.Payload, &choice)
				if choice.State == "pending" {
					s.rejected.Add(1)
					return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "injected search selection failure")
				}
			}
		}
	}
	return s.Store.Append(ctx, id, expected, commit)
}

func TestP2SelectionSearchToolsFailurePreservesInventory(t *testing.T) {
	for _, tc := range []struct {
		name, query, effect string
		fail                bool
	}{{"unknown", "select:hidden,missing", "read", false}, {"denied", "select:hidden", "write", false}, {"no-match", "unmatched-keyword", "read", false}, {"commit", "select:hidden", "read", true}} {
		t.Run(tc.name, func(t *testing.T) {
			first, searchGate, last := make(chan struct{}), make(chan struct{}), make(chan struct{})
			m := &hiddenSelectionModel{FakeModel: testkit.NewFake(testkit.Step{Gate: first, ToolCalls: []schema.FunctionToolCall{{CallID: "bootstrap", Name: "unknown", Arguments: `{}`}}}, testkit.Step{Gate: searchGate, ToolCalls: []schema.FunctionToolCall{{CallID: "search", Name: "search_tools", Arguments: `{"query":"` + tc.query + `"}`}}}, testkit.Step{Gate: last, Text: "done"}), requests: make(chan hiddenSelectionRequest, 8)}
			var runs atomic.Int32
			defs := []tools.Definition{{Name: "hidden", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: tc.effect}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ok", nil }}}
			for _, d := range tools.NewBuiltinDefinitions(tools.BuiltinOptions{}) {
				if d.Name == "search_tools" {
					defs = append(defs, d)
				}
			}
			id := agent.MustID()
			backend, err := memory.Open(id, store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			fault := &searchSelectionFaultStore{Store: backend}
			s, err := CreateAgentSession(t.Context(), Options{SessionID: id, Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: m, Store: fault, Tools: defs, ResourceScheduler: tools.NewResourceScheduler(), Policy: &agent.ResolvedPolicy{SandboxMode: "read-only"}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			receipt := submitOutput(t, s)
			hiddenSelectionNextRequest(t, m)
			if tc.name == "denied" {
				before := s.rt.manager.View()
				_, err := s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: receipt.TraceID, ToolNames: []string{"search_tools", "hidden"}, ExpectedRevision: before.LastSeq})
				pe, ok := product.AsError(err)
				if !ok || pe.Code != product.CodePermissionDenied || len(s.rt.manager.View().Selections) != len(before.Selections) {
					t.Fatalf("denied selection was accepted: %v", err)
				}
				candidates, err := s.SearchTools(t.Context(), SearchToolsRequest{TraceID: receipt.TraceID, Query: "hidden"})
				if err != nil || len(candidates) != 0 {
					t.Fatalf("denied declaration leaked into candidates: %v %v", candidates, err)
				}
			}
			_, err = s.SetActiveTools(t.Context(), SetActiveToolsRequest{TraceID: receipt.TraceID, ToolNames: []string{"search_tools"}, ExpectedRevision: s.rt.manager.View().LastSeq})
			if err != nil {
				t.Fatal(err)
			}
			close(first)
			hiddenSelectionNextRequest(t, m)
			before := s.rt.manager.View()
			fault.armed.Store(tc.fail)
			close(searchGate)
			if tc.fail {
				waitResumeCondition(t, func() bool { return s.rt.manager.Fault() != nil })
			} else {
				req := hiddenSelectionNextRequest(t, m)
				if !slices.Equal(req.tools, []string{"search_tools"}) {
					t.Fatalf("failed search expanded inventory %v", req.tools)
				}
			}
			after := s.rt.manager.View()
			if runs.Load() != 0 || len(after.Selections) != len(before.Selections) {
				t.Fatalf("selection changed: runs=%d choices=%d->%d", runs.Load(), len(before.Selections), len(after.Selections))
			}
			for id, choice := range before.Selections {
				if choice.State != after.Selections[id].State {
					t.Fatal("selection state changed")
				}
			}
			if tc.fail {
				if fault.rejected.Load() != 1 || m.Calls() != 2 {
					t.Fatalf("rejected=%d model=%d", fault.rejected.Load(), m.Calls())
				}
			} else {
				for _, call := range after.Calls {
					if call.Call.ProviderCallID == "search" {
						if call.Observation == nil || call.Observation.Status != "succeeded" {
							t.Fatalf("search observation %+v", call.Observation)
						}
						if tc.name != "no-match" {
							var result struct{ Status, Code string }
							if err := json.Unmarshal([]byte(call.Observation.Content), &result); err != nil {
								t.Fatal(err)
							}
							want := product.CodePermissionDenied
							if tc.name == "unknown" {
								want = product.CodeNotFound
							}
							if result.Status != "denied" || result.Code != want {
								t.Fatalf("search rejection %+v", result)
							}
						}
					}
				}
				if after.Traces[receipt.TraceID].Usage.ToolExecutions != 1 {
					t.Fatal("search budget counted more than once")
				}
			}
			close(last)
		})
	}
}
