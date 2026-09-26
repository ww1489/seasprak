package llm_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

type p2BrokenResponseBody struct{ io.Reader }

func (b p2BrokenResponseBody) Read(p []byte) (int, error) {
	n, e := b.Reader.Read(p)
	if e == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, e
}
func (p2BrokenResponseBody) Close() error { return nil }
func TestP2OpenAIChatControlledReadFailureIsRetryable(t *testing.T) {
	var calls atomic.Int32
	c := llm.NewCatalog(nil)
	p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		body := `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}` + "\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: p2BrokenResponseBody{strings.NewReader(body)}, Request: r}, nil
	})}, 1<<20)
	cfg := p2ChatConfig()
	p2OK(t, c.Register(cfg))
	m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, e)
	_, e = p2ChatInvoke(p2ChatContext(t, &calls), m, true, nil)
	info, ok := llm.ModelFailure(e)
	if !ok || info.Kind != "connection" {
		t.Fatal("controlled read failure classification lost")
	}
	var pe *product.Error
	if !errors.As(e, &pe) || !pe.Retryable || calls.Load() != 1 {
		t.Fatal("controlled read failure is not bounded retryable")
	}
}

func TestP2OpenAIChatOverflowCodeIsNotOrdinaryRetry(t *testing.T) {
	for _, tc := range []struct {
		status   int
		code     string
		overflow bool
	}{
		{400, "context_length_exceeded", true}, {400, "unknown", false}, {413, "unknown", false}, {429, "context_length_exceeded", false},
	} {
		var calls atomic.Int32
		c := llm.NewCatalog(nil)
		p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"message":"private too many tokens","code":%q}}`, tc.code))), Request: r}, nil
		})}, 1<<20)
		cfg := p2ChatConfig()
		cfg.Capabilities.Items[llm.CapContextOverflow] = llm.Capability{Status: llm.Verified, AdapterVersion: "offline-chat-fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline context_length_exceeded fixture"}}
		p2OK(t, c.Register(cfg))
		m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
		p2OK(t, e)
		_, e = p2ChatInvoke(p2ChatContext(t, &calls), m, true, nil)
		info, ok := llm.ModelFailure(e)
		if (ok && info.Kind == "context_overflow") != tc.overflow {
			t.Errorf("status=%d overflow=%t want=%t", tc.status, ok && info.Kind == "context_overflow", tc.overflow)
		}
		if tc.overflow && !strings.Contains(e.Error(), "no committed replacement projection") {
			t.Error("missing explicit no-projection diagnostic")
		}
	}
}

func TestP2OpenAIChatHTTPRetryClassification(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, status := range []int{400, 401, 403, 429, 500, 502, 503, 504} {
			t.Run(fmt.Sprintf("stream_%t/status_%d", stream, status), func(t *testing.T) {
				var calls atomic.Int32
				c := llm.NewCatalog(nil)
				p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"1"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"synthetic-private-provider-detail","code":"arbitrary","retryable":true}}`)), Request: r}, nil
				})}, 1<<20)
				cfg := p2ChatConfig()
				p2OK(t, c.Register(cfg))
				m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
				p2OK(t, e)
				_, e = p2ChatInvoke(p2ChatContext(t, &calls), m, stream, nil)
				var pe *product.Error
				if !errors.As(e, &pe) {
					t.Fatal("missing safe product error")
				}
				want := status == 429 || status == 500 || status == 502 || status == 503 || status == 504
				if pe.Retryable != want {
					t.Fatalf("status=%d retry=%t want=%t", status, pe.Retryable, want)
				}
				b, _ := json.Marshal(e)
				if strings.Contains(string(b), "synthetic-private") || strings.Contains(e.Error(), "synthetic-private") {
					t.Fatal("provider error leaked")
				}
				if calls.Load() != 1 {
					t.Fatalf("requests=%d", calls.Load())
				}
			})
		}
	}
}
