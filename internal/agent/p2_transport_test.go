package agent_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

type budgetWire func(*http.Request) (*http.Response, error)

func (f budgetWire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestP2TransportBudgetConcurrentAttemptsShareLimit(t *testing.T) {
	b := agent.NewBudget(config.Limits{LogicalModelRequests: 3})
	if err := b.BeginTurnID("call"); err != nil {
		t.Fatal(err)
	}
	var sent, denied atomic.Int32
	wire := llm.NewObservedTransport(budgetWire(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	}))
	var wg sync.WaitGroup
	for _, attempt := range []string{"a", "b"} {
		wg.Go(func() {
			ctx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: attempt, Purpose: "agent"}, b)
			for range 3 {
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
				res, err := wire.RoundTrip(req)
				if res != nil {
					res.Body.Close()
				}
				if err != nil {
					var pe *product.Error
					if !errors.As(err, &pe) || pe.Code != product.CodeBudgetExhausted {
						t.Error(err)
					} else {
						denied.Add(1)
					}
				}
			}
		})
	}
	wg.Wait()
	if sent.Load() != 3 || denied.Load() != 3 || b.Snapshot().TransportRequests != 3 {
		t.Fatalf("sent=%d denied=%d budget=%+v", sent.Load(), denied.Load(), b.Snapshot())
	}
}
func TestP2TransportBudgetPersistenceFailureAndCancellation(t *testing.T) {
	for _, cancelDuring := range []bool{false, true} {
		b := agent.NewBudget(config.Limits{LogicalModelRequests: 3})
		if err := b.BeginTurnID("call"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		b.SetPersist(func(agent.Usage) error {
			if cancelDuring {
				cancel()
				return nil
			}
			return product.NewError(product.CodeStorageUnavailable, "synthetic append failure")
		})
		ctx = llm.WithRequestObservation(ctx, llm.RequestIdentity{ModelCallID: "call", AttemptID: "a", Purpose: "agent"}, b)
		wire := llm.NewObservedTransport(budgetWire(func(*http.Request) (*http.Response, error) { calls++; return nil, nil }))
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
		_, err := wire.RoundTrip(req)
		cancel()
		if calls != 0 {
			t.Fatal("failed/cancelled persistence sent a request")
		}
		if cancelDuring {
			if !errors.Is(err, context.Canceled) || b.Snapshot().TransportRequests != 1 {
				t.Fatal("cancellation refunded committed occupancy")
			}
		} else {
			var pe *product.Error
			if !errors.As(err, &pe) || pe.Code != product.CodeStorageUnavailable || b.Snapshot().TransportRequests != 0 {
				t.Fatal("failed commit leaked occupancy")
			}
		}
	}
}
