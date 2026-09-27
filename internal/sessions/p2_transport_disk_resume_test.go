package sessions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

type diskTransportResumeModel struct {
	transport http.RoundTripper
	entered   chan struct{}
	release   chan struct{}
	calls     atomic.Int32
}

func (*diskTransportResumeModel) Configuration() llm.ModelConfig {
	return llm.ModelConfig{Version: "disk-transport-model-v1"}
}
func (*diskTransportResumeModel) UsesObservedTransport() bool { return true }
func (m *diskTransportResumeModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	m.calls.Add(1)
	purposes := []string{"agent"}
	if m.entered != nil {
		purposes = []string{"cache_query", "cache_create", "agent"}
	}
	for _, purpose := range purposes {
		req, err := http.NewRequestWithContext(llm.WithRequestPurpose(ctx, purpose), http.MethodPost, "https://example.invalid", nil)
		if err != nil {
			return nil, err
		}
		res, err := m.transport.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		if err := res.Body.Close(); err != nil {
			return nil, err
		}
	}
	if m.entered != nil {
		close(m.entered)
		select {
		case <-m.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// All three physical requests belonged to this still-unaccepted logical
		// call. A successful answer would advance beyond it, so it cannot stand
		// in for a resumed fourth request of the same logical call.
		return nil, &product.Error{Code: product.CodeResourceUnavailable, Message: "transient model response unavailable", Retryable: true}
	}
	return testkit.NewFake(testkit.Step{Text: "unexpected fourth send"}).Generate(ctx, input, opts...)
}
func (m *diskTransportResumeModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

type diskExhaustedTransport struct {
	session   *AgentSession
	traceID   string
	usage     agent.Usage
	transport http.RoundTripper
	sends     *atomic.Int32
	initial   *diskTransportResumeModel
	reopened  *diskTransportResumeModel
}

// A failed model response is not a resumable safe point. The original probe
// incorrectly required Pause to manufacture a checkpoint after this failure.
// See design 09 sections 4-5 and design 03 section 6: errors do not guarantee a
// checkpoint, and GenResume requires a validated one. This fixture preserves
// that scenario and its three real observed sends without claiming that public
// Resume can retry the same exhausted logical request.
func diskExhaustedTransportAfterReopen(t *testing.T) diskExhaustedTransport {
	t.Helper()
	var sends atomic.Int32
	transport := llm.NewObservedTransport(sessionWire(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
	}))
	initial := &diskTransportResumeModel{transport: transport, entered: make(chan struct{}), release: make(chan struct{})}
	opts := Options{SessionID: "disk-logical-budget", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory,
		Model: initial, GenerationFingerprint: "disk-logical-budget-v1", Limits: config.Limits{LogicalModelRequests: 3},
		// Keep the same real Agentic graph shape as the existing disk checkpoint
		// fixture. The model never requests this tool.
		Tools: []tools.Definition{{Name: "unused", Version: "1", Schema: []byte(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) { return "unused", nil }}},
	}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-initial.release:
		default:
			close(initial.release)
		}
		_ = s.Close(context.Background())
	})
	if _, ok := s.rt.opts.Store.(*jsonl.Store); !ok {
		t.Fatal("fixture is not backed by JSONL")
	}
	input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"run"}`)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-initial.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("three observed transports did not reach the response barrier")
	}
	usage := s.rt.manager.View().Traces[input.TraceID].Usage
	if sends.Load() != 3 || usage.ModelRequests != 3 || usage.TransportRequests != 3 || usage.LogicalModelCalls != 1 || usage.ModelCallID == "" {
		t.Fatalf("incorrect committed occupancy before pause: usage=%+v sends=%d", usage, sends.Load())
	}
	paused := make(chan error, 1)
	go func() { _, err := s.Pause(t.Context(), input.TraceID); paused <- err }()
	waitResumeCondition(t, func() bool { return len(s.rt.manager.View().Operations) == 1 })
	if err := s.rt.do(t.Context(), func(*runtime) error { return nil }); err != nil {
		t.Fatal(err)
	}
	close(initial.release)
	select {
	case err := <-paused:
		if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict {
			t.Fatalf("Pause must reject a failed model response without a safe checkpoint: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pause did not finish")
	}
	before := s.rt.manager.View()
	trace := before.Traces[input.TraceID]
	if len(before.Checkpoints) != 0 || trace.CheckpointID != "" || trace.State != "failed" || !trace.ExecutionStopped || !trace.Settled || trace.Usage != usage || sends.Load() != 3 || initial.calls.Load() != 1 {
		t.Fatalf("failed Pause manufactured recovery or refunded usage: trace=%+v checkpoints=%d sends=%d model=%d", trace, len(before.Checkpoints), sends.Load(), initial.calls.Load())
	}
	for _, operation := range before.Operations {
		if operation.Kind != "pause" || operation.State != "failed" {
			t.Fatalf("unexpected pause operation: %+v", operation)
		}
	}
	if len(before.ModelAttempts) != 1 || len(before.AttemptResults) != 1 {
		t.Fatal("failed attempt lost its unique lifecycle")
	}
	for _, result := range before.AttemptResults {
		if result.State != "failed" {
			t.Fatalf("failed response recorded as %s", result.State)
		}
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	resumed := &diskTransportResumeModel{transport: transport}
	opts.Model = resumed
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	if _, ok := opened.rt.opts.Store.(*jsonl.Store); !ok || opened.rt.opts.Store == s.rt.opts.Store {
		t.Fatal("Open did not rebuild through a new disk store")
	}
	if sends.Load() != 3 || initial.calls.Load() != 1 || resumed.calls.Load() != 0 || !reflect.DeepEqual(before, opened.rt.manager.View()) {
		t.Fatal("Close/Open executed a model request or changed durable failed state and usage")
	}
	return diskExhaustedTransport{session: opened, traceID: input.TraceID, usage: usage, transport: transport, sends: &sends, initial: initial, reopened: resumed}
}

func TestP2TransportDiskFailedAttemptRejectsPauseAndResume(t *testing.T) {
	f := diskExhaustedTransportAfterReopen(t)
	snapshot, err := f.session.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if eligibility := snapshot.Resume[f.traceID]; eligibility.CanResume || eligibility.Code != product.CodeIncompatibleResume {
		t.Fatalf("failed trace declared resumable: %+v", eligibility)
	}
	before := f.session.rt.manager.View()
	_, err = f.session.Resume(t.Context(), ResumeCommand{TraceID: f.traceID, ExpectedRevision: before.LastSeq})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleResume {
		t.Fatalf("Resume without a safe checkpoint must be rejected: %v", err)
	}
	if !reflect.DeepEqual(before, f.session.rt.manager.View()) || f.sends.Load() != 3 || f.initial.calls.Load() != 1 || f.reopened.calls.Load() != 0 {
		t.Fatal("rejected Resume changed durable history, refunded usage, or executed work")
	}
	t.Log("Pause=state_conflict; checkpoint refs=0; Close/Open/Resume model starts=0; persisted physical requests=3")
}

// This is a disk replay + BudgetLedger + ObservedTransport integration test,
// extending state/TestP2TransportBudgetCommitAndReopen beyond its memory store.
// Restoring a ledger here does not authorize execution of the failed Trace and
// is not a public Resume/checkpoint certification. The request is deliberately
// offered directly to the transport budget boundary, which must reject it.
func TestP2TransportDiskRestoredLedgerRejectsFourthRequest(t *testing.T) {
	f := diskExhaustedTransportAfterReopen(t)
	view := f.session.rt.manager.View()
	trace := view.Traces[f.traceID]
	ledger := agent.NewBudget(trace.Limits)
	ledger.Restore(trace.Usage)
	persistCalls := 0
	ledger.SetPersist(func(usage agent.Usage) error {
		persistCalls++
		return f.session.rt.manager.SaveTraceBudget(t.Context(), f.traceID, usage)
	})
	if err := ledger.BeginTurnID(trace.Usage.ModelCallID); err != nil {
		t.Fatal(err)
	}
	if ledger.Snapshot() != f.usage || ledger.ModelRetryAllowed() || persistCalls != 0 {
		t.Fatal("ledger reconstruction reset the original logical identity or occupancy")
	}
	before, err := f.session.rt.opts.Store.Load(t.Context(), f.session.rt.opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: trace.Usage.ModelCallID, AttemptID: "after-disk-ledger-rebuild", Purpose: "agent"}, ledger)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := f.transport.RoundTrip(request)
	if response != nil {
		_ = response.Body.Close()
		t.Error("denied transport returned a response")
	}
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeBudgetExhausted {
		t.Fatalf("fourth same-logical request must exhaust the restored budget: %v", err)
	}
	after, err := f.session.rt.opts.Store.Load(t.Context(), f.session.rt.opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if ledger.Snapshot() != f.usage || persistCalls != 0 || !reflect.DeepEqual(before, after) || f.sends.Load() != 3 || f.reopened.calls.Load() != 0 {
		t.Fatalf("fourth request changed disk/ledger or sent bytes: persists=%d sends=%d", persistCalls, f.sends.Load())
	}
	t.Log("disk-rebuilt ledger: fourth request=budget_exhausted; additional wire sends=0; new reservations=0; public Resume not exercised")
}
