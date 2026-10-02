package codeagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func summaryText() string { return strings.Join(agent.SummaryHeadings, "\nnone\n") + "\nnone" }

// An idle manual compaction with a real observed-transport catalog model must
// be metered like any model request; otherwise the transport rejects it before
// any physical request is sent.
func TestIdleCompactUsesMeteredObservedTransport(t *testing.T) {
	var summaries, agentRequests atomic.Int32
	model := p2FactoryModel(t, sessionWire(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		// JSON escapes '<', so match the encoded summary material marker.
		if strings.Contains(string(raw), `\u003chistory\u003e`) {
			// The summarization middleware calls Generate: a non-streaming request.
			summaries.Add(1)
			text, _ := json.Marshal(summaryText())
			body := `{"id":"summary","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":` + string(text) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		}
		agentRequests.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(p2FactorySSE(false))), Request: r}, nil
	}))
	s, err := CreateAgentSession(t.Context(), Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "metered", Profile: ProfileMemory, Model: model, Principal: "local", GenerationFingerprint: "metered-v1"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	for _, text := range []string{"one", "two", "three", "four"} {
		prompt(t, s, text)
	}
	receipt, err := s.Compact(t.Context(), CompactRequest{IdempotencyKey: "metered"})
	if err != nil {
		t.Fatalf("compact: %v summaries=%d agent=%d", err, summaries.Load(), agentRequests.Load())
	}
	status, _ := s.GetOperation(t.Context(), receipt.OperationID)
	if status.State != "completed" || summaries.Load() != 1 || agentRequests.Load() != 4 {
		t.Fatalf("status=%+v summaries=%d agent=%d", status, summaries.Load(), agentRequests.Load())
	}
}

func TestCompactCommitsSummaryAndProjectsIt(t *testing.T) {
	capture := &captureModel{FakeModel: testkit.NewFake()}
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "compact", Profile: ProfileMemory, Model: capture, Principal: "local", GenerationFingerprint: "compact-v1"}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first-old", "second-old", "third", "fourth"} {
		prompt(t, s, text)
	}
	if _, err = s.Compact(t.Context(), CompactRequest{}); err == nil {
		t.Fatal("compaction without idempotency key accepted")
	}
	capture.FakeModel = testkit.NewFake(testkit.Step{Text: summaryText()})
	calls := len(capture.seen)
	receipt, err := s.Compact(t.Context(), CompactRequest{IdempotencyKey: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	status, _ := s.GetOperation(t.Context(), receipt.OperationID)
	if status.State != "completed" || status.ResultRef == "" || len(capture.seen) != calls+1 {
		t.Fatalf("status=%+v calls=%d", status, len(capture.seen)-calls)
	}
	again, err := s.Compact(t.Context(), CompactRequest{IdempotencyKey: "c1"})
	if err != nil || again != receipt || len(capture.seen) != calls+1 {
		t.Fatal("replayed compaction called the model again")
	}
	capture.FakeModel = testkit.NewFake(testkit.Step{Text: "reply"})
	prompt(t, s, "after")
	last := strings.Join(capture.seen[len(capture.seen)-1], "|")
	if strings.Contains(last, "first-old") || !strings.Contains(last, "## Goal") || !strings.Contains(last, "after") {
		t.Fatalf("next request did not use the compacted projection: %s", last)
	}
	if err = s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err = OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	prompt(t, s, "reopened")
	last = strings.Join(capture.seen[len(capture.seen)-1], "|")
	if strings.Contains(last, "first-old") || !strings.Contains(last, "## Goal") {
		t.Fatalf("reopen lost the compaction projection: %s", last)
	}
}

// windowModel declares a resolved context window like a catalog model.
type windowModel struct {
	*testkit.FakeModel
	window int
}

func (w *windowModel) EffectiveOptions() llm.EffectiveOptions {
	return llm.EffectiveOptions{ContextWindowTokens: w.window, MaxOutputTokens: 16}
}

func TestContextBudgetRejectsOversizeBeforeModelCall(t *testing.T) {
	m := &windowModel{FakeModel: testkit.NewFake(), window: 64}
	s, manager, _ := controlSession(t, "window")
	s.rt.opts.Model = m
	// 4000 bytes need at least 500 tokens under every supported tokenizer.
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"` + strings.Repeat("x", 4000) + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return manager.View().Traces[in.TraceID].Settled })
	tr := manager.View().Traces[in.TraceID]
	if tr.State != "failed" || !strings.Contains(tr.Error, "budget_exhausted") || m.Calls() != 0 {
		t.Fatalf("state=%s err=%q calls=%d", tr.State, tr.Error, m.Calls())
	}
	m.window = 1 << 20
	prompt(t, s, "small")
	if m.Calls() != 1 {
		t.Fatal("request within the window was not sent")
	}
}

func TestCompactFailureKeepsOldProjection(t *testing.T) {
	capture := &captureModel{FakeModel: testkit.NewFake()}
	s, manager, _ := controlSession(t, "compact-fail")
	s.rt.opts.Model = capture
	for _, text := range []string{"a", "b", "c", "d"} {
		prompt(t, s, text)
	}
	before := manager.View()
	capture.FakeModel = testkit.NewFake(testkit.Step{Text: "## Goal\nincomplete"})
	receipt, err := s.Compact(t.Context(), CompactRequest{IdempotencyKey: "bad"})
	if err == nil {
		t.Fatal("invalid summary accepted")
	}
	after := manager.View()
	if after.LeafID != before.LeafID || len(after.Messages) != len(before.Messages) || after.Operations[receipt.OperationID].State != "failed" {
		t.Fatalf("failed candidate changed history: %+v", after.Operations[receipt.OperationID])
	}
	fresh, _, _ := controlSession(t, "compact-noop")
	if _, err = fresh.Compact(t.Context(), CompactRequest{IdempotencyKey: "n"}); err == nil || !strings.Contains(err.Error(), "no_op") {
		t.Fatalf("empty history err=%v", err)
	} else if pe, _ := product.AsError(err); pe.Code != product.CodeStateConflict {
		t.Fatal(pe.Code)
	}
}

// sizedCapture declares a mutable window and records every request's texts.
type sizedCapture struct {
	*captureModel
	window int
}

func (m *sizedCapture) EffectiveOptions() llm.EffectiveOptions {
	return llm.EffectiveOptions{ContextWindowTokens: m.window, MaxOutputTokens: 16}
}

// requestBytes measures the next request exactly as the model boundary does:
// instruction, system message, committed projection and tool schemas.
func requestBytes(t *testing.T, s *AgentSession) int {
	t.Helper()
	msgs, err := agent.ConvertToLLM(agent.ProjectHistory(s.rt.manager.View().Messages))
	if err != nil {
		t.Fatal(err)
	}
	all := append([]*schema.AgenticMessage{schema.SystemAgenticMessage(s.rt.opts.Instruction)}, msgs...)
	e, err := agent.EstimateRequest(s.rt.opts.Instruction, all, s.rt.opts.ToolInfos)
	if err != nil {
		t.Fatal(err)
	}
	return e.Bytes
}

// softWindow returns a window whose soft threshold (80% after the 16-token
// output reserve) sits at threshold request bytes.
func softWindow(threshold int) int {
	return int(float64(threshold+16) / agent.DefaultSoftRatio)
}

func freeTool(runs *atomic.Int32) tools.Definition {
	return tools.Definition{Name: "free", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		return "free-output", nil
	}}
}

func summaryCalls(c *captureModel) int {
	n := 0
	for _, texts := range c.seen {
		for _, text := range texts {
			if strings.Contains(text, "<history>") {
				n++
				break
			}
		}
	}
	return n
}

func autoOperations(v state.View, traceID string) []state.Operation {
	var out []state.Operation
	for _, op := range v.Operations {
		if op.Kind == "compact" && op.Receipt.Target == traceID {
			out = append(out, op)
		}
	}
	return out
}

func projection(t *testing.T, v state.View) string {
	t.Helper()
	msgs, err := agent.ConvertToLLM(agent.ProjectHistory(v.Messages))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(msgs)
	return string(raw)
}

func TestSoftThresholdCompactsOnceAndChargesTrace(t *testing.T) {
	m := &sizedCapture{captureModel: &captureModel{FakeModel: testkit.NewFake()}}
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "soft", Profile: ProfileMemory, Model: m, Principal: "local", GenerationFingerprint: "soft-v1"}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	for _, text := range []string{"big-one " + strings.Repeat("x", 6000), "big-two " + strings.Repeat("y", 6000), "s3", "s4"} {
		prompt(t, s, text)
	}
	if n := len(s.rt.manager.View().Operations); n != 0 || summaryCalls(m.captureModel) != 0 {
		t.Fatalf("compaction ran without a window: ops=%d", n)
	}
	// Over the threshold with the two large prompts, under it once they are summarized.
	m.window = softWindow(requestBytes(t, s) - 6000)
	m.FakeModel = testkit.NewFake(testkit.Step{Text: summaryText()}, testkit.Step{Text: "reply-five"})
	calls := len(m.seen)
	in := submitAndSettle(t, s, "s5")
	v := s.rt.manager.View()
	tr := v.Traces[in.TraceID]
	ops := autoOperations(v, in.TraceID)
	if tr.State != "completed" || len(m.seen) != calls+2 || summaryCalls(m.captureModel) != 1 || len(ops) != 1 || ops[0].State != "completed" {
		t.Fatalf("state=%s calls=%d summaries=%d ops=%+v", tr.State, len(m.seen)-calls, summaryCalls(m.captureModel), ops)
	}
	// The summary is one delegated logical call and one physical request.
	if tr.Usage.LogicalModelCalls != 2 || tr.Usage.TransportRequests != 2 {
		t.Fatalf("summary was not charged to the trace: %+v", tr.Usage)
	}
	summary := strings.Join(m.seen[calls], "|")
	agentCall := strings.Join(m.seen[calls+1], "|")
	if !strings.Contains(summary, "big-one") || strings.Contains(summary, "s4") || strings.Contains(agentCall, "big-one") || !strings.Contains(agentCall, "## Goal") || !strings.Contains(agentCall, "s5") {
		t.Fatalf("summary input or replacement projection wrong:\n%s\n%s", summary, agentCall)
	}
	// Below the threshold again: the next trace does not compact.
	m.FakeModel = testkit.NewFake(testkit.Step{Text: "reply-six"})
	next := submitAndSettle(t, s, "s6")
	if len(autoOperations(s.rt.manager.View(), next.TraceID)) != 0 || summaryCalls(m.captureModel) != 1 {
		t.Fatal("compacted projection triggered another compaction")
	}
	before := projection(t, s.rt.manager.View())
	if err = s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err = OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	if after := projection(t, s.rt.manager.View()); after != before {
		t.Fatal("reopen changed the compacted projection")
	}
}

func TestSoftThresholdRespectsDurableTraceLimit(t *testing.T) {
	var runs atomic.Int32
	m := &sizedCapture{captureModel: &captureModel{FakeModel: testkit.NewFake()}}
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "soft-limit", Profile: ProfileMemory, Model: m, Principal: "local", GenerationFingerprint: "soft-limit-v1", Limits: config.Limits{TraceCompactions: 1}, Tools: []tools.Definition{freeTool(&runs)}}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	for _, text := range []string{"a1", "a2", "a3", "a4"} {
		prompt(t, s, text)
	}
	m.window = softWindow(requestBytes(t, s) + 6000)
	// Turn 1 compacts; the large prompt stays in K, so turn 2 is still over
	// the threshold but the per-trace limit of one forbids a second summary.
	m.FakeModel = testkit.NewFake(testkit.Step{Text: summaryText()}, testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "p1", Name: "free", Arguments: `{}`}}}, testkit.Step{Text: "done"})
	calls := len(m.seen)
	in := submitAndSettle(t, s, "large "+strings.Repeat("z", 12000))
	v := s.rt.manager.View()
	if v.Traces[in.TraceID].State != "completed" || runs.Load() != 1 || len(m.seen) != calls+3 || summaryCalls(m.captureModel) != 1 || len(autoOperations(v, in.TraceID)) != 1 {
		t.Fatalf("state=%s runs=%d calls=%d summaries=%d", v.Traces[in.TraceID].State, runs.Load(), len(m.seen)-calls, summaryCalls(m.captureModel))
	}
	if err = s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err = OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	if n := s.rt.manager.TraceCompactions(in.TraceID); n != 1 {
		t.Fatalf("durable compaction count after reopen=%d", n)
	}
}

func submitAndSettle(t *testing.T, s *AgentSession, text string) agent.InputReceipt {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"text": text})
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: raw})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	return in
}

// overflowFixture is a catalog-bound OpenAI-Chat model whose overflow code is
// certified. Agent requests stream; summary requests are recognized by their
// delimited history material and answered with a valid summary.
type overflowFixture struct {
	agentCalls, summaryCalls atomic.Int32
	overflow                 map[int32]bool
}

func (f *overflowFixture) model(t *testing.T) llm.Model {
	return p2FactoryModel(t, func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		json := func(status int, body string) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		}
		if body := string(raw); strings.Contains(body, "<history>") || strings.Contains(body, `\u003chistory\u003e`) {
			f.summaryCalls.Add(1)
			content, _ := jsonMarshalString(summaryText())
			return json(200, `{"choices":[{"index":0,"message":{"role":"assistant","content":`+content+`},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`)
		}
		if n := f.agentCalls.Add(1); f.overflow[n] {
			return json(400, `{"error":{"message":"synthetic-private-service-error","code":"context_length_exceeded"}}`)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(p2FactorySSE(false))), Request: r}, nil
	}, func(cfg *llm.ModelConfig) {
		cfg.Capabilities.ContextWindowTokens = 1 << 20 // keep the soft threshold out of this path
		cfg.Capabilities.Items[llm.CapContextOverflow] = llm.Capability{Status: llm.Verified, AdapterVersion: "offline-chat-fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline context_length_exceeded fixture"}}
	})
}

func jsonMarshalString(s string) (string, error) {
	raw, err := json.Marshal(s)
	return string(raw), err
}

func overflowSession(t *testing.T, sid string, f *overflowFixture) (*AgentSession, *state.Manager) {
	t.Helper()
	backend, err := memory.Open(sid, store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, sid)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Start(Options{SessionID: sid, Profile: ProfileMemory, Store: backend, Model: f.model(t), Principal: "local"}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, manager
}

func TestCertifiedOverflowCompactsOnceAndRetriesOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overflow  map[int32]bool
		state     string
		agent     int32
		transport int
	}{
		{"recovered", map[int32]bool{6: true}, "completed", 7, 3},
		{"second_overflow_fails", map[int32]bool{6: true, 7: true}, "failed", 7, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &overflowFixture{overflow: tc.overflow}
			s, manager := overflowSession(t, "overflow-"+tc.name, f)
			for _, text := range []string{"h1", "h2", "h3", "h4", "h5"} {
				prompt(t, s, text)
			}
			if f.agentCalls.Load() != 5 || f.summaryCalls.Load() != 0 {
				t.Fatal("history setup compacted")
			}
			in := submitAndSettle(t, s, "h6")
			v := manager.View()
			tr := v.Traces[in.TraceID]
			ops := autoOperations(v, in.TraceID)
			if tr.State != tc.state || f.agentCalls.Load() != tc.agent || f.summaryCalls.Load() != 1 || len(ops) != 1 || ops[0].State != "completed" {
				t.Fatalf("state=%s agent=%d summaries=%d ops=%+v err=%q", tr.State, f.agentCalls.Load(), f.summaryCalls.Load(), ops, tr.Error)
			}
			// Two agent requests plus the summary request, all on the trace.
			if tr.Usage.TransportRequests != tc.transport || tr.Usage.LogicalModelCalls != 2 {
				t.Fatalf("usage=%+v", tr.Usage)
			}
			if tc.state == "failed" && !strings.Contains(tr.Error, "no committed replacement projection") {
				t.Fatalf("second overflow lost the original diagnostic: %q", tr.Error)
			}
			if !strings.Contains(projection(t, v), "## Goal") || strings.Contains(projection(t, v), `"h1"`) {
				t.Fatal("retry did not use the committed replacement projection")
			}
		})
	}
}

func TestOverflowWithoutCompactableRangeKeepsOriginalFailure(t *testing.T) {
	f := &overflowFixture{overflow: map[int32]bool{1: true}}
	s, manager := overflowSession(t, "overflow-noop", f)
	in := submitAndSettle(t, s, "only")
	tr := manager.View().Traces[in.TraceID]
	if tr.State != "failed" || f.agentCalls.Load() != 1 || f.summaryCalls.Load() != 0 || len(manager.View().Operations) != 0 || !strings.Contains(tr.Error, "no committed replacement projection") {
		t.Fatalf("state=%s agent=%d summaries=%d ops=%d err=%q", tr.State, f.agentCalls.Load(), f.summaryCalls.Load(), len(manager.View().Operations), tr.Error)
	}
}

func prefixText() string {
	return "## Original Request\nrequest\n## Early Progress\ntools ran\n## Context for Suffix\nnone"
}

func TestAutomaticCompactionSummarizesCurrentRequestPrefix(t *testing.T) {
	var runs atomic.Int32
	m := &sizedCapture{captureModel: &captureModel{FakeModel: testkit.NewFake()}}
	big := tools.Definition{Name: "big", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		return strings.Repeat("o", 3000), nil
	}}
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "prefix", Profile: ProfileMemory, Model: m, Principal: "local", GenerationFingerprint: "prefix-v1", Tools: []tools.Definition{big}}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	for _, text := range []string{"old-1", "old-2", "old-3", "old-4"} {
		prompt(t, s, text)
	}
	// Two tool outputs stay under the threshold, the third crosses it.
	m.window = softWindow(requestBytes(t, s) + 7500)
	call := func(id string) testkit.Step {
		return testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: id, Name: "big", Arguments: `{}`}}}
	}
	m.FakeModel = testkit.NewFake(call("p1"), call("p2"), call("p3"), testkit.Step{Text: summaryText()}, testkit.Step{Text: prefixText()}, testkit.Step{Text: "done"})
	calls := len(m.seen)
	in := submitAndSettle(t, s, "p-request")
	v := s.rt.manager.View()
	if v.Traces[in.TraceID].State != "completed" || runs.Load() != 3 || len(m.seen) != calls+6 || summaryCalls(m.captureModel) != 2 {
		t.Fatalf("state=%s runs=%d calls=%d summaries=%d err=%q", v.Traces[in.TraceID].State, runs.Load(), len(m.seen)-calls, summaryCalls(m.captureModel), v.Traces[in.TraceID].Error)
	}
	main, prefix, next := strings.Join(m.seen[calls+3], "|"), strings.Join(m.seen[calls+4], "|"), strings.Join(m.seen[calls+5], "|")
	if !strings.Contains(main, "old-1") || strings.Contains(main, "p-request") {
		t.Fatalf("H material wrong: %s", main)
	}
	if !strings.Contains(prefix, "p-request") || strings.Contains(prefix, "old-1") {
		t.Fatalf("P material wrong: %s", prefix)
	}
	if !strings.Contains(next, "## Original Request") || !strings.Contains(next, "## Goal") || strings.Contains(next, "old-1") {
		t.Fatalf("replacement projection lacks H/P summaries: %s", next)
	}
	if u := v.Traces[in.TraceID].Usage; u.LogicalModelCalls != 6 || u.TransportRequests != 6 {
		t.Fatalf("usage=%+v", u)
	}
}

type configuredCapture struct{ *captureModel }

func (configuredCapture) Configuration() llm.ModelConfig {
	return llm.ModelConfig{Version: "model-config-v1"}
}

func TestCompactDuringApprovalWaitIsDeferredThenRunsOnce(t *testing.T) {
	id := agent.MustID()
	backend, err := memory.Open(id, store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, id)
	if err != nil {
		t.Fatal(err)
	}
	capture := &captureModel{FakeModel: testkit.NewFake()}
	var runs atomic.Int32
	opts := Options{SessionID: id, Workspace: t.TempDir(), Principal: "host-user", Profile: ProfileMemory, GenerationFingerprint: "deferred-v1", Store: backend, Model: configuredCapture{capture},
		Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, Run: func(context.Context, json.RawMessage) (string, error) {
			runs.Add(1)
			return "approved result", nil
		}}}}
	if _, err := alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	s, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	for _, text := range []string{"d1", "d2", "d3", "d4"} {
		prompt(t, s, text)
	}
	capture.FakeModel = testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: summaryText()}, testkit.Step{Text: "finished"})
	calls := len(capture.seen)
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"needs approval"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool {
		tr := manager.View().Traces[in.TraceID]
		return tr.State == "paused" || terminal(tr.State)
	})
	before := manager.View()
	receipt, err := s.Compact(t.Context(), CompactRequest{IdempotencyKey: "during-approval"})
	if err != nil {
		t.Fatalf("compaction during approval was not accepted: %v", err)
	}
	after := manager.View()
	op := after.Operations[receipt.OperationID]
	if op.State != "deferred" || len(capture.seen) != calls+1 || after.LeafID != before.LeafID || after.Traces[in.TraceID].State != "paused" || after.Traces[in.TraceID].CheckpointID != before.Traces[in.TraceID].CheckpointID {
		t.Fatalf("deferral touched the waiting trace: op=%+v calls=%d", op, len(capture.seen)-calls)
	}
	cpBefore, _ := json.Marshal(before.Checkpoints)
	cpAfter, _ := json.Marshal(after.Checkpoints)
	if string(cpBefore) != string(cpAfter) {
		t.Fatal("deferral changed the checkpoint")
	}
	if again, err := s.Compact(t.Context(), CompactRequest{IdempotencyKey: "during-approval"}); err != nil || again != receipt || manager.View().Operations[receipt.OperationID].State != "deferred" {
		t.Fatal("replayed deferral changed the intent")
	}
	for _, it := range approvalSnapshot(t, s).Interactions {
		if _, err := s.RespondInteraction(t.Context(), InteractionResponse{InteractionID: it.ID, Decision: "allowed-once", ExpectedRevision: manager.View().LastSeq}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: in.TraceID, ExpectedRevision: manager.View().LastSeq}); err != nil {
		t.Fatalf("deferred intent blocked resume: %v", err)
	}
	waitResumeCondition(t, func() bool { return terminal(manager.View().Traces[in.TraceID].State) })
	v := manager.View()
	op = v.Operations[receipt.OperationID]
	if v.Traces[in.TraceID].State != "completed" || runs.Load() != 1 || op.State != "completed" || op.ResultRef == "" || len(capture.seen) != calls+3 || summaryCalls(capture) != 1 {
		t.Fatalf("state=%s runs=%d op=%+v calls=%d", v.Traces[in.TraceID].State, runs.Load(), op, len(capture.seen)-calls)
	}
	if last := strings.Join(capture.seen[len(capture.seen)-1], "|"); !strings.Contains(last, "## Goal") || strings.Contains(last, "d1") {
		t.Fatalf("resumed request did not use the deferred compaction: %s", last)
	}
	if s.rt.manager.TraceCompactions(in.TraceID) != 0 {
		t.Fatal("deferred maintenance counted against the trace limit")
	}
}

func branchText() string { return strings.Join(agent.BranchSummaryHeadings, "\nnone\n") + "\nnone" }

func TestBranchSummaryCoversOnlyUniqueSuffixAndIsAtomic(t *testing.T) {
	capture := &captureModel{FakeModel: testkit.NewFake()}
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "branch-summary", Profile: ProfileMemory, Model: capture, Principal: "local", GenerationFingerprint: "branch-summary-v1"}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	prompt(t, s, "shared-root")
	forkPoint := s.rt.manager.View().LeafID
	prompt(t, s, "main-only-1")
	prompt(t, s, "main-only-2")
	capture.FakeModel = testkit.NewFake(testkit.Step{Text: branchText()})
	calls := len(capture.seen)
	if err = s.ForkBranchWithSummary(t.Context(), "side", forkPoint, true); err != nil {
		t.Fatal(err)
	}
	material := strings.Join(capture.seen[calls], "|")
	if len(capture.seen) != calls+1 || !strings.Contains(material, "main-only-1") || !strings.Contains(material, "main-only-2") || strings.Contains(material, "shared-root") {
		t.Fatalf("branch summary material is not the unique suffix: %s", material)
	}
	v := s.rt.manager.View()
	last := v.Messages[len(v.Messages)-1]
	if v.BranchID != "side" || last.Kind != agent.KindBranchSummary || v.LeafID != last.ID || v.Nodes[last.ID].ParentID != forkPoint {
		t.Fatalf("summary not appended on the new path: branch=%s kind=%s", v.BranchID, last.Kind)
	}
	capture.FakeModel = testkit.NewFake(testkit.Step{Text: "side-reply"})
	prompt(t, s, "side-next")
	if next := strings.Join(capture.seen[len(capture.seen)-1], "|"); !strings.Contains(next, "## Explored") || strings.Contains(next, "main-only-1") {
		t.Fatalf("side request did not carry the branch summary: %s", next)
	}
	// A failed summary leaves the branch change unapplied.
	capture.FakeModel = testkit.NewFake(testkit.Step{Text: "## Explored\nincomplete"})
	before := s.rt.manager.View()
	if err = s.NavigateBranchWithSummary(t.Context(), "main", true); err == nil {
		t.Fatal("invalid branch summary accepted")
	} else if pe, _ := product.AsError(err); pe == nil || pe.Code != product.CodeInvalidArgument {
		t.Fatalf("err=%v", err)
	}
	after := s.rt.manager.View()
	if after.BranchID != "side" || after.LeafID != before.LeafID || after.LastSeq != before.LastSeq {
		t.Fatal("failed branch summary changed history")
	}
	capture.FakeModel = testkit.NewFake(testkit.Step{Err: context.DeadlineExceeded})
	if err = s.NavigateBranchWithSummary(t.Context(), "main", true); err == nil || s.rt.manager.View().LastSeq != before.LastSeq {
		t.Fatal("model failure applied the branch change")
	}
	want := projection(t, s.rt.manager.View())
	if err = s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err = OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	if got := projection(t, s.rt.manager.View()); got != want || s.rt.manager.View().BranchID != "side" {
		t.Fatal("reopen changed the summarized branch projection")
	}
}
