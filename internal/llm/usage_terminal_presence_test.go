package llm_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/llm"
)

func TestAnthropicUsageOverflowKeepsOnlyRealTerminalEvidence(t *testing.T) {
	for _, chunk := range []int{1, 7, 4096} {
		for _, limit := range []int{1, 64} {
			for _, ending := range []string{"stop", "missing", "incomplete", "embedded"} {
				t.Run(fmt.Sprintf("chunk_%d/limit_%d/%s", chunk, limit, ending), func(t *testing.T) {
					stop := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					body := "data: {\"text\":\"" + strings.Repeat("x", 256) + "\"}\n\n"
					switch ending {
					case "stop":
						body += stop
					case "incomplete":
						body += strings.TrimSuffix(stop, "\n")
					case "embedded":
						body += "data: {\"text\":\"" + strings.Repeat("x", 256) + "\",\"type\":\"message_stop\"}\n\n"
					}
					base := &usageBody{data: []byte(body), chunk: chunk, end: io.EOF}
					collector := llm.NewUsageCollector("anthropic-messages", limit)
					wrapped := llm.WrapUsageBody(base, "text/event-stream", collector)
					got, err := io.ReadAll(wrapped)
					p2OK(t, err)
					p2OK(t, wrapped.Close())
					if string(got) != body || base.closes != 1 {
						t.Fatal("read-through contract changed")
					}
					s := collector.Snapshot()
					if s.Usage != (llm.UsageRecord{}) || s.Diagnostic != "usage_buffer_limit" || s.MessageStopped != (ending == "stop") {
						t.Fatalf("snapshot=%+v", s)
					}
				})
			}
		}
	}
}

func TestAnthropicFactoryOverflowStillRequiresMessageStop(t *testing.T) {
	secret := "synthetic-presence-fixture"
	cfg := anthropicConfig()
	catalog := llm.NewCatalog(anthropicCredentialResolver(&secret))
	var physical, observed atomic.Int32
	body := factoryUsageBody("anthropic", true, "overflow")
	body = strings.ReplaceAll(body, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "")
	client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		physical.Add(1)
		return anthropicResponse(r, true, body), nil
	})}
	p2OK(t, catalog.RegisterAnthropicMessages(client, 64))
	p2OK(t, catalog.Register(cfg))
	bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	_, err = anthropicInvoke(p2ChatContext(t, &observed), bound, true)
	p2Code(t, err, "invalid_argument")
	if physical.Load() != 1 || observed.Load() != 1 {
		t.Fatal("unexpected invocation count")
	}
}
