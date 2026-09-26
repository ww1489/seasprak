package llm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/agenticclaude"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func TestP2TransportClaude429And400ThroughCatalog(t *testing.T) {
	for _, status := range []int{400, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const marker = "synthetic-private-response-marker"
			wire, reservations := 0, 0
			ctx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "logical", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error {
				reservations++
				if reservations > 2 {
					return product.NewError(product.CodeBudgetExhausted, marker)
				}
				return nil
			}))
			client := &http.Client{Transport: llm.NewObservedTransport(p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				wire++
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After-Ms": {"1"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"` + marker + `"}}`)), Request: r}, nil
			}))}
			c := llm.NewCatalog(nil)
			cfg := p2Config()
			p2OK(t, c.RegisterObservedFactory(cfg.Protocol, func(ctx context.Context, _ llm.ResolvedModelConfig) (llm.Model, error) {
				return agenticclaude.New(ctx, &agenticclaude.Config{APIKey: "synthetic-test-key", Model: "fixture", MaxTokens: 128, HTTPClient: client})
			}))
			p2OK(t, c.Register(cfg))
			m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, err)
			_, err = m.Generate(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
			wantCode, wantWire, wantReservations := product.CodeResourceUnavailable, 1, 1
			if status == 429 {
				wantCode, wantWire, wantReservations = product.CodeBudgetExhausted, 2, 3
			}
			p2Code(t, err, wantCode)
			safe, _ := product.AsError(err)
			raw, marshalErr := json.Marshal(safe)
			p2OK(t, marshalErr)
			if wire != wantWire || reservations != wantReservations || safe.Retryable || strings.Contains(string(raw), marker) || strings.Contains(err.Error(), marker) {
				t.Fatalf("classification/count/redaction mismatch: wire=%d reservations=%d", wire, reservations)
			}
		})
	}
}
