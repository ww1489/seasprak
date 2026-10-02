package codeagent

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

// The first four agent calls establish real compaction material and overflow.
// Only the fifth logical call belongs to this fixture's auxiliary summary.
// Attempt and physical failures are selected by the actual summary identity.
type p3SummaryFaultStore struct {
	store.Store
	stage    string
	summary  map[string]bool
	rejected atomic.Bool
}

func (s *p3SummaryFaultStore) Append(ctx context.Context, sid string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	for _, r := range commit.ControlRecords {
		selected := false
		switch r.Type {
		case "invocation_budget":
			var budget state.InvocationBudget
			if err := json.Unmarshal(r.Payload, &budget); err != nil {
				return store.CommitReceipt{}, err
			}
			selected = s.stage == "logical" && budget.Usage.LogicalModelCalls == 5 && budget.Usage.TransportRequests == 4 || s.stage == "physical" && budget.Usage.LastTransport.Purpose == "compaction"
		case "model_attempt":
			var attempt state.ModelAttempt
			if err := json.Unmarshal(r.Payload, &attempt); err != nil {
				return store.CommitReceipt{}, err
			}
			if attempt.Purpose == "compaction" && attempt.Scope.ParentInvocationID != "" {
				s.summary[attempt.ID] = true
				selected = s.stage == "model_attempt"
			}
		case "model_attempt_transition":
			var terminal state.ModelAttemptTransition
			if err := json.Unmarshal(r.Payload, &terminal); err != nil {
				return store.CommitReceipt{}, err
			}
			selected = s.stage == "model_attempt_transition" && s.summary[terminal.AttemptID]
		}
		if selected {
			s.rejected.Store(true)
			return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "synthetic summary append failure")
		}
	}
	return s.Store.Append(ctx, sid, expected, commit)
}

func TestP3ChildSummaryCommitFailuresKeepAcceptedEffectsAndPreventRetry(t *testing.T) {
	for _, stage := range []string{"logical", "model_attempt", "physical", "model_attempt_transition"} {
		t.Run(stage, func(t *testing.T) {
			var requests, effects atomic.Int32
			child := p2FactoryModel(t, func(r *http.Request) (*http.Response, error) {
				switch n := requests.Add(1); n {
				case 1, 2, 3:
					return p3FactoryResponse(r, 200, p3FactoryAssistant("child step", &schema.FunctionToolCall{CallID: "probe-" + strconv.Itoa(int(n)), Name: "probe", Arguments: `{}`})), nil
				case 4:
					return p3FactoryResponse(r, 400, `{"error":{"code":"context_length_exceeded","message":"synthetic overflow"}}`), nil
				case 5:
					return p3FactoryResponse(r, 200, p3FactoryAssistant(summaryText(), nil)), nil
				default:
					t.Error("failed summary commit restarted the original model call")
					return nil, product.NewError(product.CodeInternal, "unexpected test request")
				}
			}, func(cfg *llm.ModelConfig) {
				// Reach server overflow before injecting the summary commit fault.
				cfg.Capabilities.ContextWindowTokens = 32768
				cfg.Capabilities.Items[llm.CapContextOverflow] = llm.Capability{Status: llm.Verified, AdapterVersion: "offline-child-fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline context_length_exceeded fixture"}}
			})
			gate := make(chan struct{})
			var released atomic.Bool
			unblock := func() {
				if released.CompareAndSwap(false, true) {
					close(gate)
				}
			}
			defer unblock()
			rootStep := delegateCall("worker", "own task")
			rootStep.Gate = gate
			main := testkit.NewFake(rootStep, testkit.Step{Text: "parent must not continue"})
			opts := subagentOptions(agentRoots(t), "child-summary-fault-"+stage, main, []agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"probe"}}}, []tools.Definition{countedTool("probe", &effects, nil)})
			backend, err := memory.Open(opts.SessionID, store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			faults := &p3SummaryFaultStore{Store: backend, stage: stage, summary: map[string]bool{}}
			opts.Store = faults
			s := openSubagentSession(t, opts, true)
			t.Cleanup(unblock)
			in := submitPrompt(t, s, "delegate with summary persistence fault")
			frame := activityFrame(t, s)
			unblock()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			select {
			case <-frame.done:
			case <-ctx.Done():
				t.Fatal("failed summary segment did not exit")
			}
			if !faults.rejected.Load() {
				t.Fatal("actual auxiliary persistence boundary was never reached")
			}
			requireSessionCode(t, s.rt.manager.Fault(), product.CodeStorageUnavailable)
			v := s.rt.manager.View()
			inv := onlyInvocation(t, v)
			wantRequests, wantLogical, wantAttempts := 4, 5, 4
			if stage == "logical" {
				wantLogical = 4
			}
			if stage == "physical" || stage == "model_attempt_transition" {
				wantAttempts = 5
			}
			if stage == "model_attempt_transition" {
				wantRequests = 5
			}
			budget := v.InvocationBudgets[inv.ID]
			if int(requests.Load()) != wantRequests || effects.Load() != 3 || main.Calls() != 1 || len(childAttemptRecords(v, inv.ID)) != wantAttempts || len(inv.MessageIDs) != 3 || len(inv.CallIDs) != 3 || budget.Usage.LogicalModelCalls != wantLogical || budget.Usage.TransportRequests != wantRequests || v.Traces[in.TraceID].Usage.LogicalModelCalls != 1+wantLogical || v.Traces[in.TraceID].Usage.TransportRequests != 1+wantRequests {
				t.Fatalf("summary fault lost accepted work or admitted retry: stage=%s wire=%d effects=%d logical=%d attempts=%d", stage, requests.Load(), effects.Load(), budget.Usage.LogicalModelCalls, len(childAttemptRecords(v, inv.ID)))
			}
			for _, attempt := range childAttemptRecords(v, inv.ID) {
				if attempt.Purpose == "compaction" {
					if _, ended := v.AttemptResults[attempt.ID]; ended {
						t.Fatal("failed summary terminal append published a candidate")
					}
				} else if _, ended := v.AttemptResults[attempt.ID]; !ended {
					t.Fatal("summary failure removed a previously committed agent terminal")
				}
			}
			for _, call := range callsNamed(v, "probe") {
				if !call.Claimed || call.Observation == nil || !call.Observation.Executed || call.Observation.Status != "succeeded" {
					t.Fatal("summary failure discarded a committed tool effect")
				}
			}
			replayed, err := state.NewManager(backend, opts.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			after := replayed.View()
			if !reflect.DeepEqual(v.InvocationBudgets, after.InvocationBudgets) || !reflect.DeepEqual(v.AttemptResults, after.AttemptResults) || !reflect.DeepEqual(v.Calls, after.Calls) || after.Traces[in.TraceID].Usage != v.Traces[in.TraceID].Usage || int(requests.Load()) != wantRequests || effects.Load() != 3 || main.Calls() != 1 {
				t.Fatal("load-only replay refunded reservations or repeated known work")
			}
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
