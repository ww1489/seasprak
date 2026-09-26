package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/llm"
)

type p2RefusalAuthKey struct{}

func TestP2ChatRefusalCredentialIsolationAndOverflow(t *testing.T) {
	cfg := p2ChatConfig()
	cfg.NoCredentials = false
	cfg.CredentialRef = "fixture"
	c := llm.NewCatalog(p2Resolver(func(ctx context.Context, _ string) (llm.ResolvedCredential, error) {
		return llm.ResolvedCredential{Secret: ctx.Value(p2RefusalAuthKey{}).(string), Provider: cfg.Provider, Endpoint: cfg.Endpoint, AccountScope: cfg.AccountScope}, nil
	}))
	var calls atomic.Int32
	p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		var in struct {
			Stream bool `json:"stream"`
		}
		if e := json.NewDecoder(r.Body).Decode(&in); e != nil {
			return nil, e
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := []string{"原因😀 ", key[:10], key[10:]}
		if strings.HasSuffix(key, "-0") {
			parts = []string{strings.Repeat("a", 4090), key[:10], key[10:]}
		}
		return p2ChatResponse(r, in.Stream, p2RefusalBody(in.Stream, "stop", parts...)), nil
	})}, 1<<20)
	p2OK(t, c.Register(cfg))
	m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, e)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			key := fmt.Sprintf("fixture-rotating-credential-%d", i)
			ctx := context.WithValue(p2ChatContext(t, &calls), p2RefusalAuthKey{}, key)
			msg, e := p2ChatInvoke(ctx, m, i%2 == 1, nil)
			if e != nil {
				t.Error(e)
				return
			}
			raw, _ := json.Marshal(msg)
			if strings.Contains(string(raw), "fixture-rotating") {
				t.Error("request credential or a truncated prefix escaped")
			}
			want := "原因😀 [redacted]"
			if i == 0 {
				want = "[refusal reason omitted: size limit]"
			}
			if p2RefusalReason(msg) != want {
				t.Error("wrong request credential masking or total size policy")
			}
		})
	}
	wg.Wait()
	if calls.Load() != 12 {
		t.Error("physical request count mismatch")
	}
}
