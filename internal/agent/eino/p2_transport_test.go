package eino

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/agenticclaude"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

type p2Wire func(*http.Request) (*http.Response, error)

func (f p2Wire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type p2HTTPModel struct {
	client *http.Client
	cached bool
}

func (m p2HTTPModel) Generate(ctx context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
	response, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	response.Body.Close()
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, Extra: map[string]any{"seasprak.finish": "stop"}}, nil
}
func (m p2HTTPModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

func TestP2TransportValidatedSingleOccupancy(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "generate", true: "stream"}[stream], func(t *testing.T) {
			sent := 0
			client := &http.Client{Transport: llm.NewObservedTransport(p2Wire(func(r *http.Request) (*http.Response, error) {
				sent++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header), Request: r}, nil
			}))}
			budget := agent.NewBudget(config.Limits{LogicalModelRequests: 1})
			vm := NewValidatedModel(llm.WithObservedTransportModel(p2HTTPModel{client: client}), &factSink{}, budget, agent.ExecutionScope{TurnID: "call"})
			if stream {
				r, err := vm.Stream(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				r.Close()
			} else {
				if _, err := vm.Generate(t.Context(), nil); err != nil {
					t.Fatal(err)
				}
			}
			if sent != 1 || budget.Snapshot().TransportRequests != 1 || budget.Snapshot().ModelRequests != 1 {
				t.Fatalf("wire=%d budget=%+v", sent, budget.Snapshot())
			}
			_, err := vm.Generate(t.Context(), nil)
			var e *product.Error
			if !errors.As(err, &e) || e.Code != product.CodeBudgetExhausted || sent != 1 {
				t.Fatalf("exhaustion failed: %v wire=%d", err, sent)
			}
		})
	}
}

func TestP2TransportClaudeSDKAcrossValidatedAttempts(t *testing.T) {
	sent := 0
	client := &http.Client{Transport: llm.NewObservedTransport(p2Wire(func(r *http.Request) (*http.Response, error) {
		sent++
		return &http.Response{StatusCode: 500, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After-Ms": {"1"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"api_error","message":"synthetic failure"}}`)), Request: r}, nil
	}))}
	inner, err := agenticclaude.New(t.Context(), &agenticclaude.Config{APIKey: "synthetic-test-key", Model: "probe", MaxTokens: 16, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	budget := agent.NewBudget(config.Limits{LogicalModelRequests: 3})
	vm := NewValidatedModel(llm.WithObservedTransportModel(inner), &factSink{}, budget, agent.ExecutionScope{TurnID: "same-call"})
	if _, err = vm.Generate(t.Context(), []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}); err == nil {
		t.Fatal("failure accepted")
	}
	if sent != 3 || budget.Snapshot().TransportRequests != 3 {
		t.Fatalf("SDK retry count=%d budget=%+v", sent, budget.Snapshot())
	}
	_, err = vm.Generate(t.Context(), nil)
	var e *product.Error
	if !errors.As(err, &e) || e.Code != product.CodeBudgetExhausted || sent != 3 {
		t.Fatalf("new attempt exceeded logical limit: %v wire=%d", err, sent)
	}
}
