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
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/llm"
)

func p2RefusalBody(stream bool, finish string, parts ...string) string {
	marshal := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if !stream {
		return marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "refusal": strings.Join(parts, "")}, "finish_reason": finish}}})
	}
	var b strings.Builder
	for _, part := range parts {
		b.WriteString("data: " + marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "refusal": part}, "finish_reason": nil}}}) + "\n\n")
	}
	b.WriteString("data: " + marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}}) + "\n\ndata: [DONE]\n\n")
	return b.String()
}
func p2RefusalReason(msg *schema.AgenticMessage) string {
	var out string
	if msg == nil {
		return out
	}
	for _, b := range msg.ContentBlocks {
		if b != nil && b.AssistantGenText != nil && b.AssistantGenText.OpenAIExtension != nil && b.AssistantGenText.OpenAIExtension.Refusal != nil {
			out += b.AssistantGenText.OpenAIExtension.Refusal.Reason
		}
	}
	return out
}

func TestP2ChatRefusalReasonSafeAndEquivalent(t *testing.T) {
	const secret = "fixture-credential-not-for-output"
	for _, stream := range []bool{false, true} {
		for _, finish := range []string{"stop", "content_filter"} {
			t.Run(fmt.Sprintf("%t/%s", stream, finish), func(t *testing.T) {
				cfg := p2ChatConfig()
				cfg.NoCredentials = false
				cfg.CredentialRef = "fixture"
				c := llm.NewCatalog(p2Resolver(func(context.Context, string) (llm.ResolvedCredential, error) {
					return llm.ResolvedCredential{Secret: secret, Provider: cfg.Provider, Endpoint: cfg.Endpoint, AccountScope: cfg.AccountScope}, nil
				}))
				var calls atomic.Int32
				p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					return p2ChatResponse(r, stream, p2RefusalBody(stream, finish, "不能协助😀：", secret[:12], secret[12:], "\u001b安全原因")), nil
				})}, 1<<20)
				p2OK(t, c.Register(cfg))
				m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
				p2OK(t, e)
				msg, e := p2ChatInvoke(p2ChatContext(t, &calls), m, stream, nil)
				p2OK(t, e)
				reason := p2RefusalReason(msg)
				if reason != "不能协助😀：[redacted]安全原因" {
					t.Errorf("safe refusal reason missing or incorrectly normalized")
				}
				if msg.Extra["seasprak.finish"] != "refusal" || msg.Extra["seasprak.original_finish"] != finish {
					t.Error("original finish not preserved separately")
				}
				raw, _ := json.Marshal(msg)
				if strings.Contains(string(raw), secret) || calls.Load() != 1 {
					t.Error("credential leaked or request count changed")
				}
			})
		}
	}
}

func TestP2ChatRefusalBoundedAndConcurrent(t *testing.T) {
	c := llm.NewCatalog(nil)
	cfg := p2ChatConfig()
	var calls atomic.Int32
	p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		var in struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if e := json.NewDecoder(r.Body).Decode(&in); e != nil {
			return nil, e
		}
		var text string
		if e := json.Unmarshal(in.Messages[0].Content, &text); e != nil {
			var parts []struct {
				Text string `json:"text"`
			}
			if e = json.Unmarshal(in.Messages[0].Content, &parts); e != nil {
				return nil, e
			}
			for _, part := range parts {
				text += part.Text
			}
		}
		runes := []rune(text)
		split := len(runes) / 2
		return p2ChatResponse(r, in.Stream, p2RefusalBody(in.Stream, "stop", string(runes[:split]), string(runes[split:]))), nil
	})}, 1<<20)
	p2OK(t, c.Register(cfg))
	m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, e)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			text := fmt.Sprintf("原因%d😀", i)
			// Split SSE only on valid codepoint boundaries in this fixture.
			if i == 0 {
				text = strings.Repeat("a", 4095) + "😀"
			}
			if i == 1 {
				text = strings.Repeat("a", 4092) + "😀"
			}
			msg, e := p2ChatInvoke(p2ChatContext(t, &calls), m, i%2 == 1, []*schema.AgenticMessage{schema.UserAgenticMessage(text)})
			if e != nil {
				t.Error(e)
				return
			}
			reason := p2RefusalReason(msg)
			if reason == "" || len(reason) > 4096 || !utf8.ValidString(reason) {
				t.Error("refusal is missing, unbounded or invalid UTF-8")
			}
			if i != 0 && reason != text {
				t.Error("concurrent request refusal mixed")
			}
		})
	}
	wg.Wait()
	if calls.Load() != 12 {
		t.Error("physical request count mismatch")
	}
	// Many individually small SSE frames must share the total refusal bound.
	parts := make([]string, 100)
	for i := range parts {
		parts[i] = strings.Repeat("界", 40)
	}
	c2 := llm.NewCatalog(nil)
	p2ChatRegister(t, c2, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		return p2ChatResponse(r, true, p2RefusalBody(true, "stop", parts...)), nil
	})}, 1024)
	p2OK(t, c2.Register(cfg))
	m2, e := c2.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, e)
	msg, e := p2ChatInvoke(p2ChatContext(t, &calls), m2, true, nil)
	p2OK(t, e)
	if reason := p2RefusalReason(msg); reason == "" || len(reason) > 4096 || !utf8.ValidString(reason) {
		t.Error("SSE total refusal bound not enforced")
	}
}

func TestP2ChatOverflowRequiresScopedCertification(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"absent", "declared", "verified", "endpoint_mismatch", "model_mismatch", "version_mismatch", "missing_evidence", "rate_limit"} {
			t.Run(fmt.Sprintf("%t/%s", stream, kind), func(t *testing.T) {
				cfg := p2ChatConfig()
				cap := llm.Capability{Status: llm.Verified, AdapterVersion: "fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline overflow fixture"}}
				switch kind {
				case "declared":
					cap.Status = llm.Declared
				case "endpoint_mismatch":
					cap.Endpoint += "/other"
				case "model_mismatch":
					cap.Model = "other"
				case "version_mismatch":
					cap.ConfigVersion = "other"
				case "missing_evidence":
					cap.Evidence = nil
				}
				if kind != "absent" {
					cfg.Capabilities.Items[llm.CapabilityName("context_overflow")] = cap
				}
				var physical, calls atomic.Int32
				c := llm.NewCatalog(nil)
				p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					res := p2ChatResponse(r, false, `{"error":{"message":"private too many tokens","code":"context_length_exceeded"}}`)
					res.StatusCode = 400
					if kind == "rate_limit" {
						res.StatusCode = 429
					}
					return res, nil
				})}, 1<<20)
				e := c.Register(cfg)
				if strings.Contains(kind, "mismatch") || kind == "missing_evidence" {
					if e == nil {
						t.Error("invalid scoped certification registered")
					}
					if physical.Load() != 0 {
						t.Error("invalid config sent HTTP")
					}
					return
				}
				p2OK(t, e)
				m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
				p2OK(t, e)
				// Both registration input and Configuration output must be isolated.
				if ev := cfg.Capabilities.Items[llm.CapContextOverflow].Evidence; len(ev) > 0 {
					ev[0] = ""
				}
				cfg.Capabilities.Items[llm.CapabilityName("context_overflow")] = llm.Capability{Status: llm.Unsupported}
				copied := m.(interface{ Configuration() llm.ModelConfig }).Configuration()
				if ev := copied.Capabilities.Items[llm.CapabilityName("context_overflow")].Evidence; len(ev) > 0 {
					ev[0] = ""
				}
				_, e = p2ChatInvoke(p2ChatContext(t, &calls), m, stream, nil)
				info, ok := llm.ModelFailure(e)
				want := "invalid_request"
				if kind == "verified" {
					want = "context_overflow"
				}
				if kind == "rate_limit" {
					want = "rate_limit"
				}
				if !ok || info.Kind != want || physical.Load() != 1 || calls.Load() != 1 {
					t.Errorf("classification=%s want=%s HTTP=%d", info.Kind, want, physical.Load())
				}
			})
		}
	}
}
