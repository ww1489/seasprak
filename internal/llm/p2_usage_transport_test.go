package llm_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2TransportCatalogPreservesObservedDeclaration(t *testing.T) {
	cfg := p2Config()
	catalog := llm.NewCatalog(nil)
	fake := testkit.NewFake()
	p2OK(t, catalog.RegisterObservedFactory(cfg.Protocol, func(context.Context, llm.ResolvedModelConfig) (llm.Model, error) { return fake, nil }))
	p2OK(t, catalog.Register(cfg))
	bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	if !llm.UsesObservedTransport(bound) {
		t.Fatal("catalog lost transport accounting declaration")
	}
	_, err = bound.Generate(t.Context(), nil)
	p2OK(t, err)
	if fake.Calls() != 1 {
		t.Fatal("factory not invoked")
	}
	injected := llm.NewCatalog(nil)
	p2OK(t, injected.RegisterInjected(cfg, llm.WithObservedTransportModel(fake)))
	bound, err = injected.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	if !llm.UsesObservedTransport(bound) {
		t.Fatal("injected transport declaration lost")
	}
}

type usageObserver struct {
	mu        sync.Mutex
	snapshots []llm.UsageSnapshot
	requests  []llm.TransportRequest
}

func (o *usageObserver) ObserveUsage(_ context.Context, request llm.TransportRequest, s llm.UsageSnapshot) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests = append(o.requests, request)
	o.snapshots = append(o.snapshots, s)
}

func TestP2UsageObservedTransportReportsIsolatedResponses(t *testing.T) {
	observer := &usageObserver{}
	transport := llm.NewObservedTransport(p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":300,"cache_read_input_tokens":600,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":40,"ephemeral_1h_input_tokens":60},"output_tokens":2}}`)), Request: r}, nil
	}), llm.UsageCollection{Protocol: "anthropic-messages", MaxBytes: 4096})
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two"} {
		wg.Go(func() {
			ctx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: id, Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil }))
			ctx = llm.WithUsageObservation(ctx, observer)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
			res, err := transport.RoundTrip(req)
			if err != nil {
				t.Error(err)
				return
			}
			_, err = io.ReadAll(res.Body)
			if err != nil {
				t.Error(err)
			}
			res.Body.Close()
		})
	}
	wg.Wait()
	if len(observer.snapshots) != 2 || observer.requests[0].AttemptID == observer.requests[1].AttemptID {
		t.Fatal("responses mixed or reported twice")
	}
	for _, s := range observer.snapshots {
		if s.Usage.InputTotal.Value != 1000 || s.Usage.CacheWrite5Minutes.Value != 40 || s.Usage.CacheWrite1Hour.Value != 60 || s.Usage.Estimate.Known {
			t.Fatalf("wrong cache TTL or fabricated price: %+v", s)
		}
	}
}
