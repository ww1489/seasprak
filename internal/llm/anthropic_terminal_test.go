package llm_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func TestAnthropicStreamRequiresMessageStop(t *testing.T) {
	secret := "synthetic-terminal-fixture"
	cfg := anthropicConfig()
	catalog := llm.NewCatalog(anthropicCredentialResolver(&secret))
	physical := 0
	p2OK(t, catalog.RegisterAnthropicMessages(&http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		physical++
		body := anthropicStreamBody("end_turn", "partial", false, true)
		body = body[:strings.Index(body, "event: message_stop")]
		return anthropicResponse(r, true, body), nil
	})}, 1<<20))
	p2OK(t, catalog.Register(cfg))
	bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	ctx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil }))
	_, err = anthropicInvoke(ctx, bound, true)
	p2Code(t, err, product.CodeInvalidArgument)
	if physical != 1 {
		t.Fatalf("physical requests = %d", physical)
	}
}
