package llm_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/agenticclaude"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

type p2RequestObserver func(context.Context, llm.TransportRequest) error

func (f p2RequestObserver) BeforeRequest(ctx context.Context, r llm.TransportRequest) error {
	return f(ctx, r)
}

func TestP2TransportSDKHiddenRetriesStopBeforeWire(t *testing.T) {
	var mu sync.Mutex
	physical := 0
	var requests []llm.TransportRequest
	ctx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "logical", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(_ context.Context, r llm.TransportRequest) error {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r)
		if len(requests) > 2 {
			return product.NewError(product.CodeBudgetExhausted, "synthetic budget limit")
		}
		return nil
	}))
	client := &http.Client{Transport: llm.NewObservedTransport(p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		physical++
		mu.Unlock()
		return &http.Response{StatusCode: 500, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After-Ms": {"1"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"api_error","message":"synthetic failure"}}`)), Request: r}, nil
	}))}
	m, err := agenticclaude.New(ctx, &agenticclaude.Config{APIKey: "synthetic-test-key", Model: "probe", MaxTokens: 16, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Generate(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
	if err == nil {
		t.Fatal("failure accepted")
	}
	if physical != 2 || len(requests) != 3 {
		t.Fatalf("wire calls=%d occupancy calls=%d", physical, len(requests))
	}
	for i, r := range requests {
		if r.ModelCallID != "logical" || r.AttemptID != "attempt" || r.Purpose != "agent" || r.TransportAttempt != uint64(i+1) {
			t.Fatalf("request identity=%+v", r)
		}
	}
}

func TestP2TransportCanceledDuringOccupancyNeverSends(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	occupied, sent := 0, 0
	ctx = llm.WithRequestObservation(ctx, llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { occupied++; cancel(); return nil }))
	transport := llm.NewObservedTransport(p2RoundTripper(func(*http.Request) (*http.Response, error) { sent++; return nil, nil }))
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
	_, err := transport.RoundTrip(request)
	if !errors.Is(err, context.Canceled) || occupied != 1 || sent != 0 {
		t.Fatalf("error=%v occupancy=%d sent=%d", err, occupied, sent)
	}
}

func TestP2TransportConcurrentAttemptsAreIsolated(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]uint64{}
	observer := p2RequestObserver(func(_ context.Context, r llm.TransportRequest) error {
		mu.Lock()
		defer mu.Unlock()
		seen[r.AttemptID] = append(seen[r.AttemptID], r.TransportAttempt)
		return nil
	})
	transport := llm.NewObservedTransport(p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	}))
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "logical", AttemptID: id, Purpose: "agent"}, observer)
			for i := 0; i < 3; i++ {
				request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.invalid", nil)
				response, err := transport.RoundTrip(request)
				if err != nil {
					t.Error(err)
					return
				}
				response.Body.Close()
			}
		}()
	}
	wg.Wait()
	for _, id := range []string{"a", "b"} {
		if len(seen[id]) != 3 {
			t.Fatal("attempt records lost")
		}
		for i, n := range seen[id] {
			if n != uint64(i+1) {
				t.Fatal("attempt counters shared")
			}
		}
	}
}

func TestP2TransportMissingContextNeverSends(t *testing.T) {
	calls := 0
	transport := llm.NewObservedTransport(p2RoundTripper(func(*http.Request) (*http.Response, error) { calls++; return nil, nil }))
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid", nil)
	_, err := transport.RoundTrip(request)
	p2Code(t, err, product.CodeInvalidArgument)
	if calls != 0 {
		t.Fatal("unmetered request sent")
	}
}
