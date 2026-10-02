package eino

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
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

const p3AuxiliaryStreamUsageBody = `{"usage":{"prompt_tokens":9,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":1}}}`

type p3AuxiliaryStreamTerminal struct {
	Status    string                    `json:"status"`
	Message   *schema.AgenticMessage    `json:"message"`
	AttemptID string                    `json:"attemptId"`
	Details   agent.ModelAttemptDetails `json:"details"`
}

// The existing factSink records attempted writes even when err != nil. This
// wrapper passes only successful commits to it, keeping failures out of history.
// It exercises the adapter sink seam, not Session persistence or replay.
type p3AuxiliaryStreamSink struct {
	mu        sync.Mutex
	committed factSink
	scopes    []agent.ExecutionScope
	attempts  []agent.Fact
	reject    func(agent.Fact) error
}

func (s *p3AuxiliaryStreamSink) CommitFact(ctx context.Context, scope agent.ExecutionScope, fact agent.Fact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts = append(s.attempts, fact)
	if s.reject != nil {
		if err := s.reject(fact); err != nil {
			return err
		}
	}
	if err := s.committed.CommitFact(ctx, scope, fact); err != nil {
		return err
	}
	s.scopes = append(s.scopes, scope)
	return nil
}

func (s *p3AuxiliaryStreamSink) snapshot() ([]agent.Fact, []agent.ExecutionScope, []agent.Fact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.Fact(nil), s.committed.facts...), append([]agent.ExecutionScope(nil), s.scopes...), append([]agent.Fact(nil), s.attempts...)
}

type p3AuxiliaryStreamModel struct {
	fake        *testkit.FakeModel
	client      *http.Client
	streamCalls atomic.Int32
	genCalls    atomic.Int32
	beforeFinal func(context.Context) error
}

func (*p3AuxiliaryStreamModel) Configuration() llm.ModelConfig {
	return llm.ModelConfig{Version: "auxiliary-stream-v1", NoCredentials: true}
}
func (*p3AuxiliaryStreamModel) UsesObservedTransport() bool { return true }

func (m *p3AuxiliaryStreamModel) Generate(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
	m.genCalls.Add(1)
	return nil, product.NewError(product.CodeInternal, "auxiliary test requires Stream")
}

func (m *p3AuxiliaryStreamModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.streamCalls.Add(1)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/auxiliary-stream", nil)
	if err != nil {
		return nil, err
	}
	response, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}

	// FakeModel supplies the existing deterministic response script. The model
	// boundary above is counted separately: Stream never calls our Generate.
	script, err := m.fake.Stream(ctx, input, opts...)
	if script != nil {
		defer script.Close()
	}
	if err != nil {
		return nil, err
	}
	last, err := script.Recv()
	if err != nil {
		return nil, err
	}
	first := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{
		schema.NewContentBlockChunk(&schema.AssistantGenText{Text: "first "}, &schema.StreamingMeta{Index: 7}),
	}}
	var finalBlocks []*schema.ContentBlock
	for i, block := range last.ContentBlocks {
		block.StreamingMeta = &schema.StreamingMeta{Index: 7 + i}
		if block.FunctionToolCall != nil {
			// Make the tool visible before finish, without ever accepting it.
			first.ContentBlocks = append(first.ContentBlocks, block)
		} else {
			finalBlocks = append(finalBlocks, block)
		}
	}
	last.ContentBlocks = append(finalBlocks, schema.NewContentBlockChunk(&schema.Reasoning{Text: "public reasoning", Signature: "synthetic-private-signature"}, &schema.StreamingMeta{Index: 11}))
	last.Extra["synthetic.replay"] = "synthetic-private-extra"
	last.ResponseMeta = &schema.AgenticResponseMeta{
		TokenUsage: &schema.TokenUsage{PromptTokens: 9, CompletionTokens: 3, TotalTokens: 12,
			CompletionTokensDetails: schema.CompletionTokensDetails{ReasoningTokens: 1}},
		Extension: map[string]any{"opaque": "synthetic-private-extension"},
	}
	return schema.StreamReaderWithConvert(schema.StreamReaderFromArray([]int{0, 1}), func(index int) (*schema.AgenticMessage, error) {
		if index == 0 {
			return first, nil
		}
		if m.beforeFinal != nil {
			if err := m.beforeFinal(ctx); err != nil {
				return nil, err
			}
		}
		return last, nil
	}), nil
}

type p3AuxiliaryStreamParentModel struct{ calls atomic.Int32 }

func (m *p3AuxiliaryStreamParentModel) Generate(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
	m.calls.Add(1)
	return nil, product.NewError(product.CodeInternal, "parent resolved model must not be invoked")
}
func (m *p3AuxiliaryStreamParentModel) Stream(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.calls.Add(1)
	return nil, product.NewError(product.CodeInternal, "parent resolved model must not be invoked")
}

type p3AuxiliaryStreamFixture struct {
	vm            *ValidatedModel
	ctx           context.Context
	scope         agent.ExecutionScope
	parentScope   agent.ExecutionScope
	parentAttempt agent.ModelAttemptIdentity
	parentModel   *p3AuxiliaryStreamParentModel
	parentBudget  *agent.BudgetLedger
	parentUsage   agent.Usage
	budget        *agent.BudgetLedger
	inner         *p3AuxiliaryStreamModel
	sink          *p3AuxiliaryStreamSink
	physical      atomic.Int32
	requests      chan llm.TransportRequest
}

func p3NewAuxiliaryStreamFixture(t *testing.T, purpose string, step testkit.Step, body string) *p3AuxiliaryStreamFixture {
	t.Helper()
	f := &p3AuxiliaryStreamFixture{
		parentScope: agent.ExecutionScope{SessionID: "session", BranchID: "parent-branch", TraceID: "trace", InvocationID: "parent-invocation", Generation: "parent-generation", ExecutionID: "parent-execution", TurnID: "parent-turn", SelectionRevision: 3},
		scope:       agent.ExecutionScope{SessionID: "session", BranchID: "child-branch", TraceID: "trace", InvocationID: "child-invocation", ParentInvocationID: "parent-invocation", Generation: "child-generation", ExecutionID: "child-execution", TurnID: "child-turn", SelectionRevision: 7},
		parentModel: &p3AuxiliaryStreamParentModel{}, parentBudget: agent.NewBudget(config.DefaultLimits()),
		budget: agent.NewBudget(config.DefaultLimits()), sink: &p3AuxiliaryStreamSink{}, requests: make(chan llm.TransportRequest, 4),
	}
	f.parentAttempt = agent.ModelAttemptIdentity{ID: "parent-attempt", ModelCallID: "parent-turn", MessageID: "parent-message", StreamID: "parent-stream", Purpose: "agent", ModelConfigVersion: "parent-model-v1"}
	f.parentUsage = agent.Usage{LogicalModelCalls: 1, TransportRequests: 2, ModelCallID: f.parentAttempt.ModelCallID, ModelRequests: 2,
		LastTransport: llm.TransportRequest{RequestIdentity: llm.RequestIdentity{ModelCallID: f.parentAttempt.ModelCallID, AttemptID: f.parentAttempt.ID, Purpose: "agent"}, TransportAttempt: 2}}
	f.parentBudget.Restore(f.parentUsage)
	f.ctx = markTurn(WithExecutionScope(t.Context(), f.parentScope))
	f.ctx = context.WithValue(f.ctx, resolvedModelKey{}, model.AgenticModel(f.parentModel))
	f.ctx = context.WithValue(f.ctx, attemptContextKey{}, f.parentAttempt)
	f.ctx = llm.WithRequestObservation(f.ctx, f.parentUsage.LastTransport.RequestIdentity, f.parentBudget)
	step.Repeat = true
	f.inner = &p3AuxiliaryStreamModel{fake: testkit.NewFake(step)}
	f.inner.client = &http.Client{Transport: llm.NewObservedTransport(p2Wire(func(req *http.Request) (*http.Response, error) {
		f.physical.Add(1) // Only this offline wire counts physical requests.
		started, updates, terminals := f.facts(t)
		if len(started) == 0 || len(updates) != 2*(len(started)-1) || len(terminals) != len(started)-1 {
			t.Errorf("wire began before its started commit or after unexpected facts: started=%d snapshots=%d terminal=%d", len(started), len(updates), len(terminals))
		}
		identity, ok := attemptFromContext(req.Context())
		if !ok || len(started) == 0 || identity != started[len(started)-1] || ScopeFromContext(req.Context(), agent.ExecutionScope{}) != f.scope {
			t.Error("wire inherited the parent attempt or execution scope")
		}
		request := f.budget.Snapshot().LastTransport
		if request.AttemptID != identity.ID || request.ModelCallID != identity.ModelCallID || request.Purpose != purpose || request.TransportAttempt != 1 {
			t.Errorf("physical identity/ordinal=%+v attempt=%+v", request, identity)
		}
		f.requests <- request
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}), llm.UsageCollection{Protocol: "openai-chat", MaxBytes: 4096})}
	callID := ""
	if purpose == "workflow_node" {
		callID = "original-workflow-node"
	}
	var err error
	f.vm, err = NewAuxiliaryModel(f.inner, f.sink, f.budget, f.scope, purpose, callID)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *p3AuxiliaryStreamFixture) facts(t *testing.T) ([]agent.ModelAttemptIdentity, []agent.ModelStreamSnapshot, []p3AuxiliaryStreamTerminal) {
	t.Helper()
	facts, scopes, _ := f.sink.snapshot()
	var started []agent.ModelAttemptIdentity
	var updates []agent.ModelStreamSnapshot
	var terminals []p3AuxiliaryStreamTerminal
	for i, fact := range facts {
		if scopes[i] != f.scope {
			t.Errorf("fact %s scope=%+v want=%+v", fact.Kind, scopes[i], f.scope)
		}
		switch fact.Kind {
		case "model_attempt_started":
			var identity agent.ModelAttemptIdentity
			if err := json.Unmarshal(fact.Payload, &identity); err != nil {
				t.Fatal(err)
			}
			started = append(started, identity)
		case "model_stream_snapshot":
			if strings.Contains(string(fact.Payload), "synthetic-private") {
				t.Error("temporary snapshot exposed private response metadata")
			}
			var update agent.ModelStreamSnapshot
			if err := json.Unmarshal(fact.Payload, &update); err != nil {
				t.Fatal(err)
			}
			updates = append(updates, update)
		case "assistant":
			var terminal p3AuxiliaryStreamTerminal
			if err := json.Unmarshal(fact.Payload, &terminal); err != nil {
				t.Fatal(err)
			}
			terminals = append(terminals, terminal)
		default:
			t.Errorf("auxiliary Stream emitted unexpected fact %s", fact.Kind)
		}
	}
	return started, updates, terminals
}

func (f *p3AuxiliaryStreamFixture) assertCounts(t *testing.T, streams, physical, logical, perCall int) {
	t.Helper()
	usage := f.budget.Snapshot()
	if int(f.inner.streamCalls.Load()) != streams || f.inner.genCalls.Load() != 0 || f.inner.fake.Calls() != streams || int(f.physical.Load()) != physical {
		t.Errorf("invocations: Stream=%d Generate=%d testkit=%d wire=%d; want Stream/testkit=%d wire=%d", f.inner.streamCalls.Load(), f.inner.genCalls.Load(), f.inner.fake.Calls(), f.physical.Load(), streams, physical)
	}
	if usage.LogicalModelCalls != logical || usage.TransportRequests != physical || usage.ModelRequests != perCall || usage.ToolExecutions != 0 {
		t.Errorf("child budget=%+v; want logical=%d physical=%d per-call=%d tools=0", usage, logical, physical, perCall)
	}
	if f.parentModel.calls.Load() != 0 || f.parentBudget.Snapshot() != f.parentUsage || ScopeFromContext(f.ctx, agent.ExecutionScope{}) != f.parentScope || !turnStarted(f.ctx) {
		t.Error("auxiliary Stream invoked or mutated parent execution")
	}
	if inherited, ok := attemptFromContext(f.ctx); !ok || inherited != f.parentAttempt || f.ctx.Value(resolvedModelKey{}) != f.parentModel {
		t.Error("auxiliary Stream mutated the parent context's attempt or resolved model")
	}
	t.Logf("Stream=%d Generate=%d testkit=%d physical=%d logical=%d per-call=%d tools=%d parent-invocations=%d", f.inner.streamCalls.Load(), f.inner.genCalls.Load(), f.inner.fake.Calls(), f.physical.Load(), usage.LogicalModelCalls, usage.ModelRequests, usage.ToolExecutions, f.parentModel.calls.Load())
}

func (f *p3AuxiliaryStreamFixture) assertAttempt(t *testing.T, identity agent.ModelAttemptIdentity, terminal p3AuxiliaryStreamTerminal, status, code, reason string, knownUsage bool) {
	t.Helper()
	if identity.ID == "" || identity.MessageID == "" || identity.StreamID == "" || identity.ModelCallID == "" || identity.ID == f.parentAttempt.ID || identity.MessageID == f.parentAttempt.MessageID || identity.StreamID == f.parentAttempt.StreamID || identity.ModelCallID == f.parentAttempt.ModelCallID {
		t.Errorf("auxiliary identity missing or inherited: %+v", identity)
	}
	if identity.Purpose != f.vm.purpose || identity.ModelConfigVersion != "auxiliary-stream-v1" {
		t.Errorf("purpose/configuration identity=%+v", identity)
	}
	if f.vm.purpose == "workflow_node" && identity.ModelCallID != "original-workflow-node" || f.vm.purpose == "compaction" && identity.ModelCallID == f.scope.TurnID {
		t.Errorf("auxiliary logical call identity=%s purpose=%s", identity.ModelCallID, f.vm.purpose)
	}
	if terminal.AttemptID != identity.ID || terminal.Status != status || terminal.Details.FailureCode != code || terminal.Details.FailureReason != reason {
		t.Errorf("terminal identity/status/diagnostic=%+v want status=%s code=%s reason=%s", terminal, status, code, reason)
	}
	if len(terminal.Details.Usage) != 1 {
		t.Fatalf("usage items=%d want one observed physical request", len(terminal.Details.Usage))
	}
	request := <-f.requests
	if terminal.Details.Usage[0].Request != request || request.AttemptID != identity.ID || request.ModelCallID != identity.ModelCallID || request.Purpose != identity.Purpose || request.TransportAttempt != 1 {
		t.Errorf("terminal physical identity=%+v wire=%+v", terminal.Details.Usage[0].Request, request)
	}
	want := llm.UsageSnapshot{Complete: true}
	if knownUsage {
		want.Usage = llm.UsageRecord{
			InputTotal:  llm.UsageValue{Known: true, Value: 9, Source: "openai-chat/raw_usage"},
			OutputTotal: llm.UsageValue{Known: true, Value: 3, Source: "openai-chat/raw_usage"},
			CacheRead:   llm.UsageValue{Known: true, Value: 0, Source: "openai-chat/raw_usage"},
			Reasoning:   llm.UsageValue{Known: true, Value: 1, Source: "openai-chat/raw_usage"},
		}
	} else {
		want.Diagnostic = "usage_not_reported"
	}
	if got := terminal.Details.Usage[0].Snapshot; got != want {
		t.Errorf("known/unknown usage=%+v want=%+v", got, want)
	}
}

func p3AuxiliaryStreamErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var pe *product.Error
	if !errors.As(err, &pe) || pe.Code != code {
		t.Fatalf("error=%v want public code=%s", err, code)
	}
}

func TestP3AuxiliaryStreamIdentityAndAcceptance(t *testing.T) {
	for _, purpose := range []string{"compaction", "workflow_node"} {
		t.Run(purpose, func(t *testing.T) {
			f := p3NewAuxiliaryStreamFixture(t, purpose, testkit.Step{Text: "second"}, p3AuxiliaryStreamUsageBody)
			f.inner.beforeFinal = func(context.Context) error {
				started, updates, terminals := f.facts(t)
				if len(updates) != 2*len(started)-1 || len(terminals) != len(started)-1 || updates[len(updates)-1].ChunkSeq != 1 {
					t.Error("first snapshot was not visible before the current terminal commit")
				}
				return nil
			}
			for call := 1; call <= 2; call++ {
				reader, err := f.vm.Stream(f.ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				message, recvErr := reader.Recv()
				_, endErr := reader.Recv()
				reader.Close()
				if recvErr != nil || endErr != io.EOF || message == nil || hasToolCall(message) {
					t.Fatalf("accepted stream message=%v recv=%v end=%v", message, recvErr, endErr)
				}
				started, updates, terminals := f.facts(t)
				if len(started) != call || len(updates) != 2*call || len(terminals) != call {
					t.Fatalf("facts: started=%d snapshots=%d terminal=%d want=%d/%d/%d", len(started), len(updates), len(terminals), call, 2*call, call)
				}
				identity := started[call-1]
				f.assertAttempt(t, identity, terminals[call-1], "complete", "", "", true)
				for seq, update := range updates[2*(call-1) : 2*call] {
					if update.ChunkSeq != uint64(seq+1) || update.AttemptID != identity.ID || update.MessageID != identity.MessageID || update.StreamID != identity.StreamID || len(update.Blocks) != seq+1 || update.Blocks[0].BlockIndex != 7 {
						t.Errorf("snapshot sequence/identity/blocks=%+v", update)
					}
					wantText := "first "
					if seq == 1 {
						wantText = "first second"
						if update.Blocks[1].BlockIndex != 11 || update.Blocks[1].Text != "public reasoning" {
							t.Errorf("reasoning snapshot index/content=%+v", update)
						}
					}
					if update.Blocks[0].Text != wantText {
						t.Errorf("snapshot text=%q want=%q", update.Blocks[0].Text, wantText)
					}
				}
				if len(message.ContentBlocks) != 2 || message.ContentBlocks[0].AssistantGenText.Text != "first second" || message.ContentBlocks[1].Reasoning.Signature != "synthetic-private-signature" || message.Extra["synthetic.replay"] != "synthetic-private-extra" || finishReason(message) != "stop" {
					t.Error("accepted message lost complete content, signature, finish or replay metadata")
				}
				if message.ResponseMeta == nil || message.ResponseMeta.TokenUsage == nil || message.ResponseMeta.TokenUsage.PromptTokens != 9 || message.ResponseMeta.TokenUsage.CompletionTokens != 3 || message.ResponseMeta.TokenUsage.TotalTokens != 12 || message.ResponseMeta.TokenUsage.CompletionTokensDetails.ReasoningTokens != 1 || !reflect.DeepEqual(message.ResponseMeta.Extension, map[string]any{"opaque": "synthetic-private-extension"}) {
					t.Error("accepted message lost token usage or response extension")
				}
				acceptedJSON, _ := json.Marshal(message)
				persistedJSON, _ := json.Marshal(terminals[call-1].Message)
				if string(acceptedJSON) != string(persistedJSON) {
					t.Error("returned candidate differs from the successfully committed complete message")
				}
				logical, perCall := call, 1
				if purpose == "workflow_node" {
					logical, perCall = 1, call
				}
				f.assertCounts(t, call, call, logical, perCall)
			}
			started, _, _ := f.facts(t)
			if started[0].ID == started[1].ID || started[0].MessageID == started[1].MessageID || started[0].StreamID == started[1].StreamID || purpose == "compaction" && started[0].ModelCallID == started[1].ModelCallID || purpose == "workflow_node" && started[0].ModelCallID != started[1].ModelCallID {
				t.Error("attempt/message/stream identities or purpose-specific logical call reuse are incorrect")
			}
		})
	}
}

func TestP3AuxiliaryStreamRejectsUnacceptedResponses(t *testing.T) {
	for _, purpose := range []string{"compaction", "workflow_node"} {
		t.Run(purpose, func(t *testing.T) {
			for _, tc := range []struct {
				name, status, code string
				step               testkit.Step
				recvErr            error
				knownUsage         bool
				snapshots          int
			}{
				{"tools", "incomplete", product.CodeInvalidArgument, testkit.Step{Text: "second", ToolCalls: []schema.FunctionToolCall{{CallID: "visible-tool", Name: "write", Arguments: `{}`}}}, nil, true, 2},
				{"malformed_tools", "incomplete", product.CodeInvalidArgument, testkit.Step{Text: "second", ToolCalls: []schema.FunctionToolCall{{CallID: "visible-tool", Name: "write", Arguments: `{`}}}, nil, true, 2},
				{"truncated", "incomplete", product.CodeInvalidArgument, testkit.Step{Text: "second", Truncated: true}, nil, true, 2},
				{"missing_finish", "incomplete", product.CodeInvalidArgument, testkit.Step{Text: "second", NoFinish: true}, nil, false, 2},
				{"recv_error", "incomplete", product.CodeResourceUnavailable, testkit.Step{Text: "second"}, product.NewError(product.CodeResourceUnavailable, "synthetic stream receive failure"), false, 1},
				{"stream_build_error", "failed", product.CodeResourceUnavailable, testkit.Step{Err: product.NewError(product.CodeResourceUnavailable, "synthetic stream construction failure")}, nil, true, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body := "{}"
					if tc.knownUsage {
						body = p3AuxiliaryStreamUsageBody
					}
					f := p3NewAuxiliaryStreamFixture(t, purpose, tc.step, body)
					f.inner.beforeFinal = func(context.Context) error {
						_, updates, terminals := f.facts(t)
						if len(updates) != 1 || len(terminals) != 0 {
							t.Error("partial stream was accepted before final validation")
						}
						if len(tc.step.ToolCalls) > 0 && (len(updates[0].Blocks) != 2 || updates[0].Blocks[1].CallID != "visible-tool") {
							t.Error("tool candidate was not exposed as a temporary snapshot before rejection")
						}
						return tc.recvErr
					}
					reader, err := f.vm.Stream(f.ctx, nil)
					if reader != nil {
						reader.Close()
						t.Error("failed auxiliary Stream returned an executable candidate")
					}
					p3AuxiliaryStreamErrorCode(t, err, tc.code)
					started, updates, terminals := f.facts(t)
					if len(started) != 1 || len(updates) != tc.snapshots || len(terminals) != 1 {
						t.Fatalf("facts: started=%d snapshots=%d terminal=%d", len(started), len(updates), len(terminals))
					}
					f.assertAttempt(t, started[0], terminals[0], tc.status, tc.code, "model_error", tc.knownUsage)
					for i, update := range updates {
						if update.ChunkSeq != uint64(i+1) || update.AttemptID != started[0].ID || update.MessageID != started[0].MessageID || update.StreamID != started[0].StreamID {
							t.Errorf("rejected stream snapshot identity=%+v", update)
						}
					}
					partialJSON, _ := json.Marshal(terminals[0].Message)
					if hasToolCall(terminals[0].Message) || strings.Contains(string(partialJSON), "synthetic-private") || tc.snapshots > 0 && !strings.Contains(string(partialJSON), "first ") {
						t.Error("failed terminal retained tools/private metadata or discarded the safe partial")
					}
					f.assertCounts(t, 1, 1, 1, 1)
				})
			}
		})
	}
}

func TestP3AuxiliaryStreamCancellation(t *testing.T) {
	for _, purpose := range []string{"compaction", "workflow_node"} {
		t.Run(purpose, func(t *testing.T) {
			f := p3NewAuxiliaryStreamFixture(t, purpose, testkit.Step{Text: "second"}, "{}")
			ready, observed := make(chan struct{}), make(chan struct{})
			f.inner.beforeFinal = func(ctx context.Context) error {
				close(ready)
				<-ctx.Done()
				close(observed)
				return ctx.Err()
			}
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			type outcome struct {
				reader *schema.StreamReader[*schema.AgenticMessage]
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				reader, err := f.vm.Stream(ctx, nil)
				done <- outcome{reader: reader, err: err}
			}()
			select {
			case <-ready:
			case result := <-done:
				if result.reader != nil {
					result.reader.Close()
				}
				t.Fatalf("Stream returned before cancellation handshake: %v", result.err)
			case <-time.After(5 * time.Second):
				t.Fatal("Stream never reached cancellation handshake")
			}
			started, updates, terminals := f.facts(t)
			if len(started) != 1 || len(updates) != 1 || len(terminals) != 0 {
				t.Fatalf("before Cancel: started=%d snapshots=%d terminal=%d", len(started), len(updates), len(terminals))
			}
			cancel() // Public context cancellation, after actual entry and first snapshot.
			select {
			case <-observed:
			case <-time.After(5 * time.Second):
				t.Fatal("underlying Stream did not receive public cancellation")
			}
			var result outcome
			select {
			case result = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("auxiliary Stream did not exit after cancellation")
			}
			if result.reader != nil {
				result.reader.Close()
				t.Error("cancelled Stream returned an executable candidate")
			}
			if !errors.Is(result.err, context.Canceled) {
				t.Fatalf("cancellation error=%v", result.err)
			}
			started, updates, terminals = f.facts(t)
			if len(started) != 1 || len(updates) != 1 || len(terminals) != 1 {
				t.Fatalf("after Cancel: started=%d snapshots=%d terminal=%d", len(started), len(updates), len(terminals))
			}
			f.assertAttempt(t, started[0], terminals[0], "aborted", product.CodeResourceUnavailable, "cancelled", false)
			if hasToolCall(terminals[0].Message) || terminals[0].Message == nil || terminals[0].Message.ContentBlocks[0].AssistantGenText.Text != "first " {
				t.Error("cancelled terminal lost safe partial or retained tools")
			}
			f.assertCounts(t, 1, 1, 1, 1)
		})
	}
}

func TestP3AuxiliaryStreamCommitFailures(t *testing.T) {
	for _, purpose := range []string{"compaction", "workflow_node"} {
		t.Run(purpose, func(t *testing.T) {
			for _, failure := range []string{"started", "snapshot", "complete_terminal", "all_terminals"} {
				t.Run(failure, func(t *testing.T) {
					f := p3NewAuxiliaryStreamFixture(t, purpose, testkit.Step{Text: "second"}, p3AuxiliaryStreamUsageBody)
					commitErr := product.NewError(product.CodeStorageUnavailable, "synthetic auxiliary fact commit failure")
					f.sink.reject = func(fact agent.Fact) error {
						if failure == "started" && fact.Kind == "model_attempt_started" || failure == "snapshot" && fact.Kind == "model_stream_snapshot" || failure == "all_terminals" && fact.Kind == "assistant" {
							return commitErr
						}
						if failure == "complete_terminal" && fact.Kind == "assistant" {
							var terminal p3AuxiliaryStreamTerminal
							if err := json.Unmarshal(fact.Payload, &terminal); err != nil {
								return err
							}
							if terminal.Status == "complete" {
								return commitErr
							}
						}
						return nil
					}
					reader, err := f.vm.Stream(f.ctx, nil)
					if reader != nil {
						reader.Close()
						t.Error("fact failure returned an uncommitted candidate")
					}
					p3AuxiliaryStreamErrorCode(t, err, product.CodeStorageUnavailable)
					if !errors.Is(err, commitErr) {
						t.Error("fact commit cause was lost")
					}
					started, updates, terminals := f.facts(t)
					_, _, submissions := f.sink.snapshot()
					if failure == "started" {
						if len(started) != 0 || len(updates) != 0 || len(terminals) != 0 || len(submissions) != 1 || submissions[0].Kind != "model_attempt_started" {
							t.Fatal("failed registration appeared committed or emitted a terminal")
						}
						f.assertCounts(t, 0, 0, 1, 0)
						return
					}
					wantSnapshots := 2
					if failure == "snapshot" {
						wantSnapshots = 0
					}
					if len(started) != 1 || len(updates) != wantSnapshots {
						t.Fatalf("fact failure started=%d snapshots=%d", len(started), len(updates))
					}
					if failure == "all_terminals" {
						if len(terminals) != 0 {
							t.Error("rejected terminal append was reported as committed")
						}
					} else {
						if len(terminals) != 1 {
							t.Fatalf("committed failure terminal count=%d", len(terminals))
						}
						f.assertAttempt(t, started[0], terminals[0], "incomplete", product.CodeStorageUnavailable, "model_error", true)
						partialJSON, _ := json.Marshal(terminals[0].Message)
						if hasToolCall(terminals[0].Message) || strings.Contains(string(partialJSON), "synthetic-private") {
							t.Error("commit failure retained executable tools or private candidate metadata")
						}
					}
					var attemptedTerminals []p3AuxiliaryStreamTerminal
					for _, fact := range submissions {
						if fact.Kind == "assistant" {
							var terminal p3AuxiliaryStreamTerminal
							if err := json.Unmarshal(fact.Payload, &terminal); err != nil {
								t.Fatal(err)
							}
							attemptedTerminals = append(attemptedTerminals, terminal)
						}
					}
					wantAttempts := 2
					if failure == "snapshot" {
						wantAttempts = 1
					}
					if len(attemptedTerminals) != wantAttempts || attemptedTerminals[len(attemptedTerminals)-1].Status != "incomplete" {
						t.Fatal("fact failure terminal submission sequence is incorrect")
					}
					if wantAttempts == 2 && attemptedTerminals[0].Status != "complete" {
						t.Error("complete terminal failure seam was not actually invoked")
					}
					for _, terminal := range attemptedTerminals {
						if terminal.AttemptID != started[0].ID {
							t.Error("terminal submission failure inherited a parent attempt")
						}
					}
					f.assertCounts(t, 1, 1, 1, 1)
					t.Logf("terminal submissions=%d successful terminal commits=%d", len(attemptedTerminals), len(terminals))
				})
			}
		})
	}
}
