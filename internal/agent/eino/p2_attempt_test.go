package eino

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/llm"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
)

type p2AttemptFailureModel struct {
	buildErr    error
	readErr     error
	role        schema.AgenticRoleType
	requests    int
	nilReader   bool
	buildReader *schema.StreamReader[*schema.AgenticMessage]
}

func (m *p2AttemptFailureModel) Generate(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
	panic("stream required")
}
func (m *p2AttemptFailureModel) Stream(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.requests++
	if m.buildErr != nil {
		return m.buildReader, m.buildErr
	}
	if m.nilReader {
		return nil, nil
	}
	return schema.StreamReaderWithConvert(schema.StreamReaderFromArray([]int{0, 1}), func(i int) (*schema.AgenticMessage, error) {
		if i == 0 {
			return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "partial"})}}, nil
		}
		if m.readErr != nil {
			return nil, m.readErr
		}
		return &schema.AgenticMessage{Role: m.role}, nil
	}), nil
}
func TestP2AttemptFailedPayloadExcludesPrivateMetadata(t *testing.T) {
	sink := &factSink{}
	vm := NewValidatedModel(malformedModel{}, sink, nil, agent.ExecutionScope{})
	ctx, err := vm.requestContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	msg := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, Extra: map[string]any{"seasprak.finish": "length", "private": "synthetic-private-signature"}, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.Reasoning{Text: "visible", Signature: "synthetic-private-signature"})}}
	if err := vm.fail(ctx, msg, "incomplete", errors.New("synthetic-private-error")); err == nil {
		t.Fatal("failure missing")
	}
	for _, f := range sink.facts {
		if strings.Contains(string(f.Payload), "synthetic-private") {
			t.Error("private failure payload persisted")
		}
	}
}

func TestP2AttemptRefusalDiagnosticDoesNotOverrideCancellation(t *testing.T) {
	sink := &factSink{}
	vm := NewValidatedModel(malformedModel{}, sink, nil, agent.ExecutionScope{})
	ctx, err := vm.requestContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	msg := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, Extra: map[string]any{"seasprak.finish": "refusal"}}
	if err := vm.fail(ctx, msg, "incomplete", context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	for _, fact := range sink.facts {
		if fact.Kind == "assistant" {
			var body struct {
				Status  string                    `json:"status"`
				Details agent.ModelAttemptDetails `json:"details"`
			}
			if err := json.Unmarshal(fact.Payload, &body); err != nil {
				t.Fatal(err)
			}
			if body.Status != "aborted" || body.Details.FailureReason != "cancelled" {
				t.Fatal("refusal overrode cancellation diagnostic")
			}
		}
	}
}

func TestP2AttemptConcatFailureKeepsSafePartial(t *testing.T) {
	sink := &factSink{}
	inner := &p2AttemptFailureModel{role: schema.AgenticRoleType("synthetic-private-role")}
	vm := NewValidatedModel(inner, sink, nil, agent.ExecutionScope{})
	_, err := vm.Stream(t.Context(), nil)
	if err == nil || strings.Contains(err.Error(), "synthetic-private") {
		t.Error("concat failure leaked provider fields")
	}
	for _, f := range sink.facts {
		if f.Kind == "assistant" && !strings.Contains(string(f.Payload), "partial") {
			t.Error("concat failure discarded prior safe partial")
		}
	}
}

func TestP2AttemptStreamBuildReaderClosed(t *testing.T) {
	reader, writer := schema.Pipe[*schema.AgenticMessage](1)
	defer writer.Close()
	sink := &factSink{}
	vm := NewValidatedModel(&p2AttemptFailureModel{buildErr: errors.New("build failed"), buildReader: reader}, sink, nil, agent.ExecutionScope{})
	if _, err := vm.Stream(t.Context(), nil); err == nil {
		t.Fatal("missing build failure")
	}
	if !writer.Send(nil, nil) {
		reader.Close()
		t.Fatal("reader accompanying build failure was not closed")
	}
}

func TestP2AttemptStreamFailuresHaveOneTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		model        *p2AttemptFailureModel
	}{
		{"nil_reader", "failed", &p2AttemptFailureModel{nilReader: true}},
		{"build", "failed", &p2AttemptFailureModel{buildErr: errors.New("synthetic stream build error")}},
		{"recv", "incomplete", &p2AttemptFailureModel{readErr: errors.New("synthetic recv error")}},
		{"concat", "incomplete", &p2AttemptFailureModel{role: schema.AgenticRoleTypeUser}},
		{"unknown_eof", "incomplete", &p2AttemptFailureModel{role: schema.AgenticRoleTypeAssistant}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &factSink{}
			vm := NewValidatedModel(tc.model, sink, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{})
			r, err := vm.Stream(t.Context(), nil)
			if r != nil {
				r.Close()
			}
			if err == nil || tc.model.requests != 1 {
				t.Fatalf("stream error=%v requests=%d", err, tc.model.requests)
			}
			terminals := 0
			for _, fact := range sink.facts {
				if fact.Kind != "assistant" {
					continue
				}
				terminals++
				var body struct {
					Status    string `json:"status"`
					AttemptID string `json:"attemptId"`
				}
				if err := json.Unmarshal(fact.Payload, &body); err != nil {
					t.Fatal(err)
				}
				if body.Status != tc.status || body.AttemptID == "" {
					t.Fatal("stream failure terminal identity/status mismatch")
				}
			}
			if terminals != 1 {
				t.Fatalf("stream terminal count=%d", terminals)
			}
		})
	}
}

func TestP2AttemptPersistenceFailureNeverRetriesEvenIfBackendMarksTransient(t *testing.T) {
	transient := product.NewError(product.CodeResourceUnavailable, "backend failure")
	transient.Retryable = true
	vm := NewValidatedModel(malformedModel{}, &factSink{err: transient}, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{})
	_, err := vm.Generate(t.Context(), nil)
	if retryDecision(t.Context(), &adk.TypedRetryContext[*schema.AgenticMessage]{RetryAttempt: 1, Err: err}, nil).Retry {
		t.Fatal("registration persistence error authorized retry")
	}
}

func TestP2AttemptRegistrationFailureStartsNoHTTP(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: llm.NewObservedTransport(p2Wire(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	}))}
	budg := agent.NewBudget(config.DefaultLimits())
	sink := &factSink{err: product.NewError(product.CodeStorageUnavailable, "registration failed")}
	vm := NewValidatedModel(llm.WithObservedTransportModel(p2HTTPModel{client: client}), sink, budg, agent.ExecutionScope{TurnID: "turn"})
	_, err := vm.Generate(t.Context(), nil)
	var pe *product.Error
	ok := errors.As(err, &pe)
	if !ok || pe.Code != product.CodeStorageUnavailable || requests != 0 || budg.Snapshot().TransportRequests != 0 || len(sink.facts) != 1 || sink.facts[0].Kind != "model_attempt_started" {
		t.Fatalf("failed registration: err=%v requests=%d facts=%d", err, requests, len(sink.facts))
	}
}

func TestP2AttemptCancellationHasOneTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sink := &factSink{}
	vm := NewValidatedModel(&p2BoundaryStream{beforeEnd: cancel}, sink, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{})
	reader, err := vm.Stream(ctx, nil)
	if reader != nil {
		reader.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel outcome=%v", err)
	}
	terminals := 0
	for _, fact := range sink.facts {
		if fact.Kind != "assistant" {
			continue
		}
		terminals++
		var body struct {
			Status    string `json:"status"`
			AttemptID string `json:"attemptId"`
		}
		if err := json.Unmarshal(fact.Payload, &body); err != nil {
			t.Fatal(err)
		}
		if body.Status != "aborted" || body.AttemptID == "" {
			t.Fatal("cancellation lost attempt identity or aborted state")
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal count=%d", terminals)
	}
}

func TestP2AttemptRetryRejectsAcceptedCancelledAndStorageErrors(t *testing.T) {
	transient := product.NewError(product.CodeResourceUnavailable, "temporary")
	transient.Retryable = true
	for _, tc := range []struct {
		name   string
		err    error
		output *schema.AgenticMessage
	}{
		{"accepted", transient, &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant}},
		{"cancelled", errors.Join(transient, context.Canceled), nil},
		{"storage", errors.Join(transient, product.NewError(product.CodeStorageUnavailable, "commit failed")), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := retryDecision(t.Context(), &adk.TypedRetryContext[*schema.AgenticMessage]{RetryAttempt: 1, Err: tc.err, OutputMessage: tc.output}, nil)
			if decision.Retry {
				t.Fatal("unsafe model retry authorized")
			}
		})
	}
}
