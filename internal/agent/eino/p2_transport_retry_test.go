package eino

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/agenticclaude"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func TestP2TransportClaudeSDKAttemptsShareThreeRequests(t *testing.T) {
	sent := 0
	client := &http.Client{Transport: llm.NewObservedTransport(p2Wire(func(r *http.Request) (*http.Response, error) {
		sent++
		status := 500
		kind := "api_error"
		if sent == 1 {
			status = 401
			kind = "authentication_error"
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After-Ms": {"1"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"` + kind + `","message":"synthetic failure"}}`)), Request: r}, nil
	}))}
	inner, err := agenticclaude.New(t.Context(), &agenticclaude.Config{APIKey: "synthetic-test-key", Model: "probe", MaxTokens: 16, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	b := agent.NewBudget(config.Limits{LogicalModelRequests: 3})
	var records []llm.TransportRequest
	b.SetPersist(func(u agent.Usage) error {
		if u.LastTransport.AttemptID != "" {
			records = append(records, u.LastTransport)
		}
		return nil
	})
	vm := NewValidatedModel(llm.WithObservedTransportModel(inner), &factSink{}, b, agent.ExecutionScope{TurnID: "call"})
	_, err = vm.Generate(context.Background(), []*schema.AgenticMessage{schema.UserAgenticMessage("first")})
	if err == nil || sent != 1 {
		t.Fatalf("first attempt requests=%d error=%v", sent, err)
	}
	_, err = vm.Generate(context.Background(), []*schema.AgenticMessage{schema.UserAgenticMessage("second")})
	var pe *product.Error
	if !errors.As(err, &pe) || pe.Code != product.CodeBudgetExhausted || sent != 3 || len(records) != 3 {
		t.Fatalf("shared limit failed: wire=%d records=%d error=%v", sent, len(records), err)
	}
	if records[0].AttemptID == records[1].AttemptID || records[1].AttemptID != records[2].AttemptID || records[1].TransportAttempt != 1 || records[2].TransportAttempt != 2 {
		t.Fatalf("attempt/physical identities not isolated: %+v", records)
	}
	if b.Snapshot().LogicalModelCalls != 1 || b.Snapshot().ModelRequests != 3 {
		t.Fatal("outer attempt reset the logical budget")
	}
}
