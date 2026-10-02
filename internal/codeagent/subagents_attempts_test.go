package codeagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

// All physical requests use the real observed transport and an offline wire.
// The wire consumes real bytes so attempt usage is supplementary evidence,
// rather than a fabricated terminal usage observation.
type p3ChildAttemptModel struct {
	*testkit.FakeModel
	client     *http.Client
	generates  atomic.Int32
	physical   atomic.Int32
	registered atomic.Int32
	manager    *state.Manager
	beforeWire func(*http.Request)
}

func (*p3ChildAttemptModel) UsesObservedTransport() bool { return true }
func (*p3ChildAttemptModel) Configuration() llm.ModelConfig {
	return llm.ModelConfig{Model: "synthetic-child", Version: "child-v1"}
}

func (m *p3ChildAttemptModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	m.generates.Add(1)
	scope := einorun.ScopeFromContext(ctx, agent.ExecutionScope{})
	view := m.manager.View()
	for _, attempt := range view.ModelAttempts {
		if attempt.Scope == scope && attempt.State == "started" && attempt.ModelConfigVersion == "child-v1" {
			if _, ended := view.AttemptResults[attempt.ID]; !ended {
				m.registered.Add(1)
				break
			}
		}
	}
	for _, purpose := range []string{"cache_query", "agent"} {
		req, _ := http.NewRequestWithContext(llm.WithRequestPurpose(ctx, purpose), http.MethodPost, "https://example.invalid", nil)
		response, err := m.client.Do(req)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		if err != nil || closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
	}
	msg, err := m.FakeModel.Generate(ctx, in, opts...)
	if msg != nil {
		msg.ResponseMeta = &schema.AgenticResponseMeta{TokenUsage: &schema.TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}, Extension: map[string]any{"provider": "synthetic-child", "revision": float64(3)}}
		msg.Extra["p3-private-child"] = map[string]any{"items": []any{"probe-only", float64(17)}, "signed": "synthetic-metadata"}
	}
	return msg, err
}

func (m *p3ChildAttemptModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil || msg == nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

func newP3ChildAttemptModel(steps ...testkit.Step) *p3ChildAttemptModel {
	m := &p3ChildAttemptModel{FakeModel: testkit.NewFake(steps...)}
	m.client = &http.Client{Transport: llm.NewObservedTransport(sessionWire(func(r *http.Request) (*http.Response, error) {
		if m.beforeWire != nil {
			m.beforeWire(r)
		}
		m.physical.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)), Request: r}, nil
	}), llm.UsageCollection{Protocol: "openai-chat", MaxBytes: 4096})}
	return m
}

func childAttemptRecords(v state.View, invocationID string) []state.ModelAttempt {
	var attempts []state.ModelAttempt
	for _, initial := range v.ModelAttempts {
		if initial.Scope.InvocationID == invocationID {
			attempts = append(attempts, initial)
		}
	}
	return attempts
}

func TestP3ChildAttemptsRegisterAndCommitPrivateTerminal(t *testing.T) {
	for _, outcome := range []string{"accepted", "failed", "incomplete"} {
		t.Run(outcome, func(t *testing.T) {
			var effects atomic.Int32
			step := testkit.Step{Text: "child intermediate", ToolCalls: []schema.FunctionToolCall{{CallID: "child-provider", Name: "probe", Arguments: `{}`}}}
			if outcome == "failed" {
				step.Err = errors.New("synthetic child model failure")
			}
			if outcome == "incomplete" {
				step.Truncated = true
			}
			child := newP3ChildAttemptModel(step, testkit.Step{Text: "child final"})
			main := testkit.NewFake(delegateCall("worker", "own task"), testkit.Step{Text: "parent final"})
			opts := subagentOptions(agentRoots(t), "child-attempt-"+outcome, main,
				[]agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"probe"}}},
				[]tools.Definition{countedTool("probe", &effects, nil)})
			s := openSubagentSession(t, opts, true)
			child.manager = s.rt.manager
			in := submitPrompt(t, s, "delegate")
			waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
			v := s.rt.manager.View()
			inv := onlyInvocation(t, v)
			attempts := childAttemptRecords(v, inv.ID)
			want := 1
			if outcome == "accepted" {
				want = 2
			}
			if len(attempts) != want || int(child.registered.Load()) != want || int(child.generates.Load()) != want || int(child.physical.Load()) != 2*want {
				t.Fatalf("child attempts were not registered before requests: attempts=%d registered=%d Generate=%d physical=%d", len(attempts), child.registered.Load(), child.generates.Load(), child.physical.Load())
			}
			if (outcome == "accepted" && effects.Load() != 1) || (outcome != "accepted" && effects.Load() != 0) {
				t.Fatal("unaccepted child response started a tool")
			}
			stored, err := s.rt.opts.Store.Load(t.Context(), s.rt.opts.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			terminals := map[string]int{}
			for _, commit := range stored.Commits {
				for _, record := range commit.ControlRecords {
					if record.Type != "model_attempt_transition" {
						continue
					}
					var result state.ModelAttemptTransition
					if json.Unmarshal(record.Payload, &result) != nil {
						t.Fatal("invalid terminal")
					}
					initial := v.ModelAttempts[result.AttemptID]
					if initial.Scope.InvocationID != inv.ID {
						continue
					}
					terminals[result.AttemptID]++
					if len(commit.Entries) != 0 || initial.Scope.TurnID != "" || initial.Scope.SelectionRevision != 0 || initial.Scope.ParentInvocationID != inv.ParentInvocationID || initial.MessageID == "" || initial.StreamID == "" || result.State != outcome {
						t.Fatal("child terminal borrowed parent history or lost its original identity")
					}
					details := v.AttemptDetails[result.DiagnosticRef]
					if len(details.Usage) != 2 {
						t.Fatal("child physical usage evidence was dropped")
					}
					for i, usage := range details.Usage {
						if usage.Request.AttemptID != initial.ID || usage.Request.ModelCallID != initial.ModelCallID || usage.Request.TransportAttempt != uint64(i+1) || !usage.Snapshot.Usage.InputTotal.Known || usage.Snapshot.Usage.InputTotal.Value != 7 {
							t.Fatal("usage lost original request identity or cumulative evidence")
						}
					}
					private := false
					for _, candidate := range commit.ControlRecords {
						if candidate.Type != "invocation_message" || candidate.ID != initial.MessageID {
							continue
						}
						var msg agent.AgentMessage
						if json.Unmarshal(candidate.Payload, &msg) != nil {
							t.Fatal("invalid private candidate")
						}
						private = msg.Scope.InvocationID == inv.ID && msg.Standard != nil
						wantMeta := &schema.AgenticResponseMeta{TokenUsage: &schema.TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}, Extension: map[string]any{"provider": "synthetic-child", "revision": float64(3)}}
						if outcome == "accepted" && !reflect.DeepEqual(msg.Standard.ResponseMeta, wantMeta) {
							t.Fatal("complete private child candidate lost response metadata")
						}
						if outcome != "accepted" && msg.Standard.ResponseMeta != nil {
							t.Fatal("failed diagnostic bypassed the existing model metadata allowlist")
						}
						if outcome == "accepted" && !reflect.DeepEqual(msg.Standard.Extra["p3-private-child"], map[string]any{"items": []any{"probe-only", float64(17)}, "signed": "synthetic-metadata"}) {
							t.Fatal("complete private candidate lost extension metadata")
						}
					}
					// An establishment error returns no readable Stream. Only received
					// content can become a private diagnostic candidate.
					if private != (outcome != "failed") {
						t.Fatal("child Stream candidate differs from content actually received")
					}
				}
			}
			for _, attempt := range attempts {
				if terminals[attempt.ID] != 1 {
					t.Fatal("child attempt has no unique terminal")
				}
			}
			if usage := v.Traces[in.TraceID].Usage; usage.LogicalModelCalls != 2+want || usage.TransportRequests != 2+2*want {
				t.Fatalf("child terminal refunded or double charged usage: %+v", usage)
			}
			for _, msg := range v.Messages {
				if msg.Scope.InvocationID == inv.ID {
					t.Fatal("child candidate entered parent branch")
				}
			}
			for _, turn := range v.Turns {
				if turn.InvocationID == inv.ID {
					t.Fatal("child attempt created a parent Turn")
				}
			}
			public, _ := json.Marshal(v.Events)
			if strings.Contains(string(public), "p3-private-child") || strings.Contains(string(public), "child intermediate") {
				t.Fatal("private child candidate entered public events")
			}
			before := v.Traces[in.TraceID].Usage
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			s = openSubagentSession(t, opts, false)
			reopened := s.rt.manager.View()
			if reopened.Traces[in.TraceID].Usage != before || !reflect.DeepEqual(v.ModelAttempts, reopened.ModelAttempts) || !reflect.DeepEqual(v.AttemptResults, reopened.AttemptResults) || !reflect.DeepEqual(v.AttemptDetails, reopened.AttemptDetails) || child.physical.Load() != int32(2*want) {
				t.Fatal("reopen changed child attempts, evidence or cumulative reservations")
			}
		})
	}
}

// Failure injection targets actual child records, never parent attempts.
type p3ChildAttemptFaultStore struct {
	store.Store
	kind     string
	mu       sync.Mutex
	children map[string]bool
	failed   bool
}

func (s *p3ChildAttemptFaultStore) Append(ctx context.Context, sid string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range commit.ControlRecords {
		child := false
		switch record.Type {
		case "model_attempt":
			var attempt state.ModelAttempt
			_ = json.Unmarshal(record.Payload, &attempt)
			child = attempt.Scope.ParentInvocationID != ""
			if child {
				s.children[attempt.ID] = true
			}
		case "model_attempt_transition":
			var result state.ModelAttemptTransition
			_ = json.Unmarshal(record.Payload, &result)
			child = s.children[result.AttemptID]
		}
		if child && record.Type == s.kind {
			s.failed = true
			return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "synthetic child attempt append failure")
		}
	}
	return s.Store.Append(ctx, sid, expected, commit)
}

func (s *p3ChildAttemptFaultStore) didFail() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.failed }

func TestP3ChildAttemptsAppendFailureStartsNoUnacceptedWork(t *testing.T) {
	for _, kind := range []string{"model_attempt", "model_attempt_transition"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			child := newP3ChildAttemptModel(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "child-provider", Name: "probe", Arguments: `{}`}}})
			main := testkit.NewFake(delegateCall("worker", "own task"), testkit.Step{Text: "parent final"})
			faults := &p3ChildAttemptFaultStore{kind: kind, children: map[string]bool{}}
			opts := subagentOptions(agentRoots(t), "child-fault-"+kind, main,
				[]agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"probe"}}}, []tools.Definition{countedTool("probe", &effects, nil)})
			backend, err := memory.Open(opts.SessionID, store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			faults.Store = backend
			opts.Store = faults
			s := openSubagentSession(t, opts, true)
			child.manager = s.rt.manager
			in := submitPrompt(t, s, "delegate")
			waitFor(t, func() bool { return faults.didFail() || s.rt.manager.View().Traces[in.TraceID].Settled })
			if !faults.didFail() {
				t.Fatal("child attempt persistence was never invoked")
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			v := s.rt.manager.View()
			wantModels, wantPhysical, wantAttempts := int32(0), int32(0), 0
			if kind == "model_attempt_transition" {
				wantModels, wantPhysical, wantAttempts = 1, 2, 1
			}
			inv := onlyInvocation(t, v)
			attempts := childAttemptRecords(v, inv.ID)
			if child.generates.Load() != wantModels || child.physical.Load() != wantPhysical || effects.Load() != 0 || len(attempts) != wantAttempts || main.Calls() != 1 || len(inv.CallIDs) != 0 {
				t.Fatal("failed child commit started a model/tool or accepted its candidate")
			}
			for _, attempt := range attempts {
				if _, ended := v.AttemptResults[attempt.ID]; ended {
					t.Fatal("failed child terminal append became visible")
				}
			}
		})
	}
}
