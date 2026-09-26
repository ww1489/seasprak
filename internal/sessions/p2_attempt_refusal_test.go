package sessions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

type p2RefusalCredentialResolver struct {
	cfg    llm.ModelConfig
	secret string
}

func (r p2RefusalCredentialResolver) Resolve(context.Context, string) (llm.ResolvedCredential, error) {
	return llm.ResolvedCredential{Secret: r.secret, Provider: r.cfg.Provider, Endpoint: r.cfg.Endpoint, AccountScope: r.cfg.AccountScope}, nil
}

func TestP2AttemptRefusalPersistsSafeReasonAndNeverProjects(t *testing.T) {
	for _, finish := range []string{"stop", "content_filter"} {
		t.Run(finish, func(t *testing.T) {
			const secret = "fixture-refusal-credential"
			const reason = "不能执行：安全原因😀 "
			backend, err := memory.Open("refusal", store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			manager, err := state.NewManager(backend, "refusal")
			if err != nil {
				t.Fatal(err)
			}
			d := llm.Capability{Status: llm.Declared, Evidence: []string{"offline fixture"}}
			cfg := llm.ModelConfig{Provider: "fixture", Protocol: "openai-chat", Model: "fixture", Endpoint: "https://example.invalid/v1", Version: "v1", AccountScope: "local", CredentialRef: "fixture", Capabilities: llm.ModelCapabilities{Items: map[llm.CapabilityName]llm.Capability{llm.CapText: d, llm.CapTextStream: d, llm.CapTools: d, llm.CapContextWindow: d, llm.CapOutputLimit: d, llm.CapPhysicalRequestMetering: d}, ContextWindowTokens: 4096, MaxOutputTokens: 512}, Parameters: llm.ModelParameters{MaxOutputTokens: 128, PolicyVersion: "fixture"}}
			c := llm.NewCatalog(p2RefusalCredentialResolver{cfg, secret})
			var requests, runs atomic.Int32
			reached, release := make(chan struct{}), make(chan struct{})
			err = c.RegisterOpenAIChat(&http.Client{Transport: sessionWire(func(r *http.Request) (*http.Response, error) {
				requests.Add(1)
				close(reached)
				<-release
				frame := func(delta map[string]any, finish any) string {
					raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
					return "data: " + string(raw) + "\n\n"
				}
				body := frame(map[string]any{"role": "assistant", "refusal": reason + secret[:8], "tool_calls": []any{map[string]any{"index": 0, "id": "provider", "type": "function", "function": map[string]any{"name": "write", "arguments": "{}"}}}, "signature": "synthetic-private-signature"}, nil) + frame(map[string]any{"refusal": secret[8:]}, nil) + frame(map[string]any{}, finish) + "data: [DONE]\n\n"
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Private": {"synthetic-private-header"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if err = c.Register(cfg); err != nil {
				t.Fatal(err)
			}
			m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
			if err != nil {
				t.Fatal(err)
			}
			opts := Options{SessionID: "refusal", Profile: ProfileMemory, Store: backend, Model: m, Tools: []tools.Definition{{Name: "write", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "written", nil }}}}
			if _, err = alignTools(&opts); err != nil {
				t.Fatal(err)
			}
			s, err := Start(opts, manager, "gen")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			sub := s.SubscribeEvents(config.DefaultLimits())
			defer sub.Close()
			receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"write"}`)})
			if err != nil {
				t.Fatal(err)
			}
			<-reached
			f := activityFrame(t, s)
			close(release)
			activityWait(t, f)
			v := manager.View()
			if requests.Load() != 1 || runs.Load() != 0 || len(v.Calls) != 0 || len(v.ModelAttempts) != 1 || len(v.AttemptResults) != 1 || v.Traces[receipt.TraceID].State != "failed" {
				t.Fatal("refusal was retried, accepted or executed a tool")
			}
			for _, terminal := range v.AttemptResults {
				if terminal.State != "incomplete" || terminal.FinishReason != "refusal" {
					t.Error("refusal terminal incorrect")
				}
				raw, _ := json.Marshal(v.AttemptDetails[terminal.DiagnosticRef])
				var details map[string]any
				_ = json.Unmarshal(raw, &details)
				if details["refusalReason"] != reason+"[redacted]" || details["originalFinishReason"] != finish {
					t.Error("safe refusal and original finish absent from durable attempt diagnostic")
				}
			}
			incomplete := 0
			for _, msg := range v.Messages {
				if msg.Kind == agent.KindAssistant {
					if msg.Status != agent.StatusIncomplete {
						t.Error("refusal message accepted")
					}
					incomplete++
				}
			}
			if incomplete != 1 {
				t.Error("refusal must have exactly one incomplete message")
			}
			projected, err := agent.ConvertToLLM(v.Messages)
			if err != nil {
				t.Fatal(err)
			}
			projection, _ := json.Marshal(projected)
			if strings.Contains(string(projection), "不能执行") || strings.Contains(string(projection), "provider") {
				t.Error("incomplete refusal entered model projection")
			}
			stored, err := backend.Load(t.Context(), "refusal")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(stored)
			if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "synthetic-private") {
				t.Error("private response data entered journal")
			}
			sub.sub.mu.Lock()
			for _, item := range sub.sub.queue {
				raw, _ := json.Marshal(item.event)
				if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "synthetic-private") {
					t.Error("private response data entered event")
				}
			}
			sub.sub.mu.Unlock()
			reopened, err := state.NewManager(backend, "refusal")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ = json.Marshal(reopened.View().AttemptDetails)
			if !strings.Contains(string(raw), reason) {
				t.Error("refusal diagnostic lost on reopen")
			}
		})
	}
}
