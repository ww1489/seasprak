package codeagent

import (
	"context"
	"encoding/json"
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
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

// Count direct product calls while keeping the real Catalog/OpenAIChat factory,
// observed transport and SSE parser. No terminal facts are manufactured here.
type p3SessionStreamModel struct {
	llm.Model
	generates atomic.Int32
	streams   atomic.Int32
}

func (*p3SessionStreamModel) UsesObservedTransport() bool { return true }
func (m *p3SessionStreamModel) Configuration() llm.ModelConfig {
	return m.Model.(interface{ Configuration() llm.ModelConfig }).Configuration()
}
func (m *p3SessionStreamModel) EffectiveOptions() llm.EffectiveOptions {
	return m.Model.(interface{ EffectiveOptions() llm.EffectiveOptions }).EffectiveOptions()
}
func (m *p3SessionStreamModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	m.generates.Add(1)
	return m.Model.Generate(ctx, in, opts...)
}
func (m *p3SessionStreamModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.streams.Add(1)
	return m.Model.Stream(ctx, in, opts...)
}

func p3SessionSSE(text string, call *schema.FunctionToolCall, complete bool) string {
	frame := func(value any) string {
		raw, _ := json.Marshal(value)
		return "data: " + string(raw) + "\n\n"
	}
	delta := map[string]any{"role": "assistant", "content": text}
	finish := "stop"
	if call != nil {
		finish = "tool_calls"
		delta["tool_calls"] = []any{map[string]any{"index": 0, "id": call.CallID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments[:1]}}}
	}
	choice := func(delta any, reason any) string {
		return frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}})
	}
	body := choice(delta, nil)
	if !complete {
		return body // Actual partial bytes, without finish or DONE.
	}
	if call != nil {
		body += choice(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": call.Arguments[1:]}}}}, nil)
	}
	return body + choice(map[string]any{}, finish) + frame(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}}) + "data: [DONE]\n\n"
}

func p3SessionStreamResponse(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

// Read actual committed records at the last boundary before physical dispatch.
func p3AssertSessionStreamReservation(t *testing.T, s *AgentSession, r *http.Request, ordinal int) {
	t.Helper()
	scope := einorun.ScopeFromContext(r.Context(), agent.ExecutionScope{})
	v := s.rt.manager.View()
	budget := v.InvocationBudgets[scope.InvocationID]
	request := budget.Usage.LastTransport
	attempt := v.ModelAttempts[request.AttemptID]
	if scope.ParentInvocationID == "" || scope.TurnID != "" || budget.Scope != scope || budget.Usage.TransportRequests != ordinal || budget.Calls[request.ModelCallID].LastTransport != request || attempt.Scope != scope || attempt.ModelCallID != request.ModelCallID || attempt.Purpose != "agent" || attempt.ModelConfigVersion != "fixture-v2" || attempt.StreamID == "" || attempt.MessageID == "" {
		t.Error("child Stream reached wire without its original durable identity and reservation")
		return
	}
	stored, err := s.rt.opts.Store.Load(context.WithoutCancel(r.Context()), s.rt.opts.SessionID)
	if err != nil {
		t.Error("cannot inspect actual child Stream reservation")
		return
	}
	for i := len(stored.Commits) - 1; i >= 0; i-- {
		commit := stored.Commits[i]
		child, parent := false, false
		for _, record := range commit.ControlRecords {
			switch record.Type {
			case "invocation_budget":
				var saved state.InvocationBudget
				if json.Unmarshal(record.Payload, &saved) == nil && saved.ID == scope.InvocationID && saved.Usage.LastTransport == request {
					child = true
				}
			case "trace":
				var tr state.TraceState
				if json.Unmarshal(record.Payload, &tr) == nil && tr.ID == scope.TraceID && tr.Usage.TransportRequests == 1+ordinal && tr.Usage.ModelCallID != request.ModelCallID && tr.Usage.ModelRequests == 1 {
					parent = true
				}
			}
		}
		if child {
			if !parent || len(commit.Entries) != 0 {
				t.Error("child Stream reservation was not jointly committed with the parent Trace")
			}
			for _, record := range commit.ControlRecords {
				if record.Type == "turn" {
					t.Error("child Stream reservation borrowed a parent Turn")
				}
			}
			return
		}
	}
	t.Error("child Stream request has no committed joint reservation")
}

func p3AssertStreamReopen(t *testing.T, s *AgentSession, opts Options, before state.View, child *p3SessionStreamModel, requests int32) {
	t.Helper()
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened := openSubagentSession(t, opts, false)
	after := reopened.rt.manager.View()
	if !reflect.DeepEqual(before.InvocationBudgets, after.InvocationBudgets) || !reflect.DeepEqual(before.ModelAttempts, after.ModelAttempts) || !reflect.DeepEqual(before.AttemptResults, after.AttemptResults) || !reflect.DeepEqual(before.AttemptDetails, after.AttemptDetails) || !reflect.DeepEqual(before.InvocationMessages, after.InvocationMessages) || !reflect.DeepEqual(before.Calls, after.Calls) || !reflect.DeepEqual(before.Traces, after.Traces) || child.streams.Load() != requests || child.generates.Load() != 0 {
		t.Fatal("public JSONL reopen changed Stream identities, occupancy, candidates or invocation counts")
	}
}

func TestP3ChildStreamPersistsTerminalToolsRetryAndReopen(t *testing.T) {
	for _, scenario := range []string{"accepted", "failed", "incomplete", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			var requests, effects atomic.Int32
			var s *AgentSession
			tool := &schema.FunctionToolCall{CallID: "original-stream-provider", Name: "probe", Arguments: `{}`}
			child := &p3SessionStreamModel{Model: p2FactoryModel(t, func(r *http.Request) (*http.Response, error) {
				n := requests.Add(1)
				p3AssertSessionStreamReservation(t, s, r, int(n))
				var body struct {
					Stream bool `json:"stream"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || !body.Stream {
					t.Error("actual delegated factory request did not enable Stream")
					return p3FactoryResponse(r, 200, p3FactoryAssistant("child final", nil)), nil
				}
				if scenario == "failed" || scenario == "retry" && n == 1 {
					status := 401
					if scenario == "retry" {
						status = 503
					}
					return p3FactoryResponse(r, status, `{"error":{"code":"service_error","message":"synthetic-private-stream-error"}}`), nil
				}
				if scenario == "incomplete" {
					response := p3SessionStreamResponse(r, "")
					response.Body = p2AttemptBrokenBody{strings.NewReader(p3SessionSSE("private-child-prefix", tool, false))}
					return response, nil
				}
				first := int32(1)
				if scenario == "retry" {
					first = 2
				}
				if n == first {
					return p3SessionStreamResponse(r, p3SessionSSE("private-child-candidate", tool, true)), nil
				}
				if n != first+1 || effects.Load() != 1 {
					t.Error("stream retry repeated a tool or admitted excess wire")
				}
				return p3SessionStreamResponse(r, p3SessionSSE("child final", nil, true)), nil
			})}
			main := testkit.NewFake(delegateCall("worker", "private task"), testkit.Step{Text: "parent final"})
			opts := subagentOptions(agentRoots(t), "child-stream-"+scenario, main, []agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"probe"}}}, []tools.Definition{countedTool("probe", &effects, nil)})
			if scenario == "incomplete" {
				opts.Limits = config.Limits{LogicalModelRequests: 1}
			}
			s = openSubagentSession(t, opts, true)
			in := submitPrompt(t, s, "delegate with Stream")
			waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
			v := s.rt.manager.View()
			inv := onlyInvocation(t, v)
			wantRequests, wantLogical, wantEffects, wantState := int32(2), 2, int32(1), "completed"
			if scenario == "retry" {
				wantRequests = 3
			}
			if scenario == "failed" || scenario == "incomplete" {
				wantRequests, wantLogical, wantEffects, wantState = 1, 1, 0, "failed"
			}
			attempts := childAttemptRecords(v, inv.ID)
			budget := v.InvocationBudgets[inv.ID]
			if requests.Load() != wantRequests || child.streams.Load() != wantRequests || child.generates.Load() != 0 || effects.Load() != wantEffects || inv.State != wantState || inv.ModelCalls != wantLogical || len(attempts) != int(wantRequests) || main.Calls() != 2 || len(v.Turns) != 2 || budget.Usage.LogicalModelCalls != wantLogical || budget.Usage.TransportRequests != int(wantRequests) || v.Traces[in.TraceID].Usage.LogicalModelCalls != 2+wantLogical || v.Traces[in.TraceID].Usage.TransportRequests != 2+int(wantRequests) {
				t.Fatalf("Session child Stream path/counts differ: scenario=%s Generate=%d Stream=%d wire=%d effects=%d invocation=%s calls=%d", scenario, child.generates.Load(), child.streams.Load(), requests.Load(), effects.Load(), inv.State, inv.ModelCalls)
			}
			out := delegateOutcome(t, delegateRecord(t, s))
			if out.InvocationID != inv.ID || out.Agent != "worker" || out.Status != wantState {
				t.Fatal("Stream result lost its public invocation and target identity")
			}
			if wantState == "failed" {
				if out.Code != product.CodeResourceUnavailable || out.Result != "" || len(inv.MessageIDs) != 0 {
					t.Fatal("failed Stream returned content, an incorrect error code or active candidates")
				}
			} else if out.Code != "" || out.Result != "child final" || len(inv.MessageIDs) != wantLogical {
				t.Fatal("successful Stream returned an incorrect final result or active candidate count")
			}
			stored, err := s.rt.opts.Store.Load(t.Context(), opts.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			failedCall := ""
			for _, attempt := range attempts {
				terminal := v.AttemptResults[attempt.ID]
				count := 0
				for _, commit := range stored.Commits {
					for _, record := range commit.ControlRecords {
						if record.Type != "model_attempt_transition" {
							continue
						}
						var saved state.ModelAttemptTransition
						if json.Unmarshal(record.Payload, &saved) != nil || saved.AttemptID != attempt.ID {
							continue
						}
						count++
						if len(commit.Entries) != 0 || saved.Revision != 2 || saved.ExpectedRevision != 1 {
							t.Fatal("Stream terminal rewrote history or created another revision")
						}
						if saved.State == "accepted" {
							candidate, callCount := false, 0
							for _, joint := range commit.ControlRecords {
								candidate = candidate || joint.Type == "invocation_message" && joint.ID == attempt.MessageID
								if joint.Type == "tool_call" {
									callCount++
								}
							}
							msg := v.InvocationMessages[attempt.MessageID]
							if !candidate || msg.Standard == nil || msg.Status != agent.StatusComplete || msg.Standard.ResponseMeta == nil || msg.Standard.ResponseMeta.TokenUsage == nil {
								t.Fatal("accepted Stream candidate lost joint commit or full usage metadata")
							}
							if strings.Contains(assistantStandardText(msg.Standard), "private-child-candidate") && callCount != 1 {
								t.Fatal("complete Stream tool was not admitted atomically with its candidate")
							}
						}
					}
				}
				if count != 1 || attempt.Scope.TurnID != "" || attempt.Scope.ParentInvocationID != inv.ParentInvocationID || terminal.State == "" {
					t.Fatal("Stream attempt lost its original scope or unique terminal")
				}
				details := v.AttemptDetails[terminal.UsageRef]
				if len(details.Usage) != 1 || details.Usage[0].Request.AttemptID != attempt.ID || details.Usage[0].Request.ModelCallID != attempt.ModelCallID || details.Usage[0].Request.TransportAttempt != 1 {
					t.Fatal("Stream terminal lost original physical usage identity")
				}
				if terminal.State == "accepted" && (!details.Usage[0].Snapshot.Usage.InputTotal.Known || details.Usage[0].Snapshot.Usage.InputTotal.Value != 7) {
					t.Fatal("Stream actual-byte usage was discarded")
				}
				if terminal.State != "accepted" {
					reason := "authentication"
					if scenario == "retry" {
						reason = "service_unavailable"
					} else if scenario == "incomplete" {
						reason = "connection"
					}
					if details.FailureCode != product.CodeResourceUnavailable || details.FailureReason != reason || details.Usage[0].Snapshot.Usage.InputTotal.Known {
						t.Fatal("Stream failure lost trusted classification or fabricated usage")
					}
					msg, saved := v.InvocationMessages[attempt.MessageID]
					if scenario == "incomplete" {
						if !saved || msg.Standard == nil || msg.Status != agent.StatusIncomplete || assistantStandardText(msg.Standard) != "private-child-prefix" || msg.Standard.ResponseMeta != nil {
							t.Fatal("received partial Stream was not retained as a sanitized private diagnostic")
						}
						for _, block := range msg.Standard.ContentBlocks {
							if block.FunctionToolCall != nil {
								t.Fatal("partial Stream diagnostic retained executable tool fragments")
							}
						}
					} else if saved {
						t.Fatal("Stream establishment failure manufactured an unread candidate")
					}
				}
				if terminal.State == "failed" {
					failedCall = attempt.ModelCallID
				}
				if scenario == "incomplete" && terminal.State != "incomplete" || scenario == "failed" && terminal.State != "failed" {
					t.Fatal("failed Stream terminal was accepted")
				}
			}
			if scenario == "retry" {
				retried := 0
				for _, attempt := range attempts {
					if attempt.ModelCallID == failedCall && attempt.Attempt == 2 && v.AttemptResults[attempt.ID].State == "accepted" {
						retried++
					}
				}
				if retried != 1 || budget.Calls[failedCall].Requests != 2 {
					t.Fatal("Stream retry reset logical identity or refunded request occupancy")
				}
			}
			probes := callsNamed(v, "probe")
			if len(inv.CallIDs) != int(wantEffects) || len(probes) != int(wantEffects) {
				t.Fatal("failed or partial Stream admitted provisional tools")
			}
			for _, probe := range probes {
				frozen, ok := v.FrozenExecutions["execution:"+probe.Call.CallID]
				if probe.Scope.InvocationID != inv.ID || probe.Scope.ParentInvocationID != inv.ParentInvocationID || probe.Call.ProviderCallID != tool.CallID || probe.Call.Name != tool.Name || probe.Call.Arguments != tool.Arguments || probe.Call.Hash == "" || !probe.Claimed || probe.Observation == nil || !probe.Observation.Executed || probe.Observation.Status != "succeeded" || !ok || frozen.CallID != probe.Call.CallID || frozen.Scope != probe.Scope || frozen.ProviderCallID != tool.CallID || frozen.Tool != tool.Name || frozen.ToolVersion != "1" || string(frozen.FinalArguments) != tool.Arguments {
					t.Fatal("fragmented Stream tool lost its original frozen identity, arguments or confirmed result")
				}
			}
			snapshot, err := s.Snapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			public, _ := json.Marshal([]any{v.Messages, v.Events, snapshot})
			if strings.Contains(string(public), "private-child-") || strings.Contains(string(public), "synthetic-private-stream-error") {
				t.Fatal("child Stream candidate, partial text or provider diagnostic entered public state")
			}
			saved, _ := json.Marshal(v)
			if strings.Contains(string(saved), "synthetic-private-stream-error") {
				t.Fatal("provider Stream error text escaped its private diagnostic allowlist")
			}
			p3AssertStreamReopen(t, s, opts, v, child, wantRequests)
			if requests.Load() != wantRequests || effects.Load() != wantEffects || main.Calls() != 2 {
				t.Fatal("public reopen executed Stream or tool work")
			}
		})
	}
}

func assistantStandardText(msg *schema.AgenticMessage) string {
	var b strings.Builder
	for _, block := range msg.ContentBlocks {
		if block != nil && block.AssistantGenText != nil {
			b.WriteString(block.AssistantGenText.Text)
		}
	}
	return b.String()
}

// The real HTTP body's reader waits for cancellation after a partial SSE frame.
// Exit is observed separately from receiving a cancellation request.
type p3SessionBlockedStreamBody struct {
	io.Reader
	ctx     context.Context
	entered chan struct{}
	exited  chan struct{}
	once    sync.Once
}

func (b *p3SessionBlockedStreamBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err != io.EOF {
		return n, err
	}
	b.once.Do(func() { close(b.entered) })
	<-b.ctx.Done()
	select {
	case <-b.exited:
	default:
		close(b.exited)
	}
	return n, b.ctx.Err()
}
func (*p3SessionBlockedStreamBody) Close() error { return nil }

func TestP3ChildStreamCancelWaitsForWireExitAndReopensWithoutWork(t *testing.T) {
	entered, exited := make(chan struct{}), make(chan struct{})
	var requests, effects atomic.Int32
	var s *AgentSession
	child := &p3SessionStreamModel{Model: p2FactoryModel(t, func(r *http.Request) (*http.Response, error) {
		n := requests.Add(1)
		p3AssertSessionStreamReservation(t, s, r, int(n))
		response := p3SessionStreamResponse(r, "")
		response.Body = &p3SessionBlockedStreamBody{Reader: strings.NewReader(p3SessionSSE("private-child-prefix", &schema.FunctionToolCall{CallID: "cancelled-stream-tool", Name: "probe", Arguments: `{}`}, false)), ctx: r.Context(), entered: entered, exited: exited}
		return response, nil
	})}
	main := testkit.NewFake(delegateCall("worker", "private task"), testkit.Step{Text: "parent must not continue"})
	opts := subagentOptions(agentRoots(t), "child-stream-cancel", main, []agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"probe"}}}, []tools.Definition{countedTool("probe", &effects, nil)})
	s = openSubagentSession(t, opts, true)
	in := submitPrompt(t, s, "cancel actual child Stream")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child Stream did not enter its actual body read")
	}
	if err := s.Cancel(t.Context(), in.TraceID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("public Cancel returned before actual body read exited")
	}
	v := s.rt.manager.View()
	inv := onlyInvocation(t, v)
	attempts := childAttemptRecords(v, inv.ID)
	if requests.Load() != 1 || child.streams.Load() != 1 || child.generates.Load() != 0 || main.Calls() != 1 || effects.Load() != 0 || inv.State != "cancelled" || len(attempts) != 1 || len(inv.CallIDs) != 0 || len(inv.MessageIDs) != 0 || v.Traces[in.TraceID].State != "cancelled" || !v.Traces[in.TraceID].ExecutionStopped || !v.Traces[in.TraceID].Settled || len(v.Turns) != 1 || v.InvocationBudgets[inv.ID].Usage.TransportRequests != 1 || v.Traces[in.TraceID].Usage.TransportRequests != 2 {
		t.Fatal("cancelled Stream lost occupancy, accepted a late tool or continued its parent")
	}
	terminal := v.AttemptResults[attempts[0].ID]
	if terminal.State != "aborted" || terminal.Revision != 2 || len(v.AttemptDetails[terminal.UsageRef].Usage) != 1 || v.AttemptDetails[terminal.UsageRef].Usage[0].Snapshot.Usage.InputTotal.Known {
		t.Fatal("cancelled partial Stream lost its unique terminal or fabricated known usage")
	}
	p3AssertStreamReopen(t, s, opts, v, child, 1)
	if requests.Load() != 1 || effects.Load() != 0 || main.Calls() != 1 {
		t.Fatal("reopen executed cancelled Stream work")
	}
}
