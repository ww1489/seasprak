package sessions

import (
	"context"
	"encoding/json"
	"github.com/ww1489/seasprak/internal/config"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

func p2FactoryModel(t *testing.T, wire sessionWire, configure ...func(*llm.ModelConfig)) llm.Model {
	t.Helper()
	c := llm.NewCatalog(nil)
	if err := c.RegisterOpenAIChat(&http.Client{Transport: wire}, 1<<20); err != nil {
		t.Fatal(err)
	}
	declared := llm.Capability{Status: llm.Declared, Evidence: []string{"offline attempt fixture"}}
	cfg := llm.ModelConfig{Provider: "fixture", Protocol: "openai-chat", Model: "fixture", Version: "fixture-v2", Endpoint: "https://example.invalid/v1", NoCredentials: true, AccountScope: "local", Capabilities: llm.ModelCapabilities{Items: map[llm.CapabilityName]llm.Capability{llm.CapText: declared, llm.CapTextStream: declared, llm.CapTools: declared, llm.CapContextWindow: declared, llm.CapOutputLimit: declared, llm.CapPhysicalRequestMetering: declared}, ContextWindowTokens: 4096, MaxOutputTokens: 512}, Parameters: llm.ModelParameters{MaxOutputTokens: 128, PolicyVersion: "fixture"}}
	for _, apply := range configure {
		apply(&cfg)
	}
	if err := c.Register(cfg); err != nil {
		t.Fatal(err)
	}
	m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func p2FactorySSE(tool bool) string {
	content := `"content":"done"`
	finish := "stop"
	if tool {
		content = `"tool_calls":[{"index":0,"id":"provider-call","type":"function","function":{"name":"write","arguments":"{}"}}]`
		finish = "tool_calls"
	}
	return "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"," + content + "},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"" + finish + "\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"
}

type p2AttemptBrokenBody struct{ io.Reader }

func (b p2AttemptBrokenBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}
func (p2AttemptBrokenBody) Close() error { return nil }

func TestP2AttemptChatFactoryRetriesWithoutRepeatingTools(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statuses  []int
		tool      bool
		wantRuns  int32
		wantState string
	}{
		{"partial_then_success", []int{200, 200}, false, 0, "completed"},
		{"retry_after_exceeds_cap", []int{503}, false, 0, "failed"},
		{"overflow", []int{400}, false, 0, "failed"},
		{"overflow_unverified", []int{400}, false, 0, "failed"},
		{"activity_limited", []int{503}, false, 0, "failed"},
		{"429", []int{429, 200}, false, 0, "completed"}, {"503", []int{503, 200}, false, 0, "completed"},
		{"tool_then_503", []int{200, 503, 200}, true, 1, "completed"},
		{"exhausted", []int{503, 503, 503}, false, 0, "failed"}, {"unauthenticated", []int{401}, false, 0, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, err := memory.Open("factory-retry", store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			manager, err := state.NewManager(backend, "factory-retry")
			if err != nil {
				t.Fatal(err)
			}
			reached, release := make(chan struct{}), make(chan struct{})
			var requests, runs atomic.Int32
			m := p2FactoryModel(t, func(r *http.Request) (*http.Response, error) {
				n := int(requests.Add(1))
				if n == 1 {
					close(reached)
					<-release
				}
				if n > len(tc.statuses) {
					t.Error("unexpected extra physical request")
					return nil, context.Canceled
				}
				status := tc.statuses[n-1]
				body := p2FactorySSE(tc.tool && n == 1)
				kind := "text/event-stream"
				if status != 200 {
					body = `{"error":{"message":"synthetic-private-service-error","code":"service_error"}}`
					kind = "application/json"
				}
				header := http.Header{"Content-Type": {kind}}
				if tc.name == "retry_after_exceeds_cap" {
					header.Set("Retry-After", "11")
				}
				if strings.HasPrefix(tc.name, "overflow") {
					body = `{"error":{"message":"synthetic-private-service-error","code":"context_length_exceeded"}}`
				}
				var responseBody io.ReadCloser = io.NopCloser(strings.NewReader(body))
				if tc.name == "partial_then_success" && n == 1 {
					partial := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"failed-prefix\"},\"finish_reason\":null}]}\n\n" +
						`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"partial-call","type":"function","function":{"name":"write","arguments":"{"}}]},"finish_reason":null}]}` + "\n\n"
					responseBody = p2AttemptBrokenBody{strings.NewReader(partial)}
				}
				if n > 1 && runs.Load() != tc.wantRuns {
					t.Error("partial tool ran or completed tool was lost before retry")
				}
				return &http.Response{StatusCode: status, Header: header, Body: responseBody, Request: r}, nil
			}, func(cfg *llm.ModelConfig) {
				if tc.name == "overflow" {
					cfg.Capabilities.Items[llm.CapContextOverflow] = llm.Capability{Status: llm.Verified, AdapterVersion: "offline-chat-fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline context_length_exceeded fixture"}}
				}
			})
			opts := Options{SessionID: "factory-retry", Profile: ProfileMemory, Store: backend, Model: m, Tools: []tools.Definition{{Name: "write", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "written", nil }}}}
			if tc.name == "activity_limited" {
				opts.Limits = config.Limits{ActivityBudget: 50 * time.Millisecond}
			}
			if _, err := alignTools(&opts); err != nil {
				t.Fatal(err)
			}
			session, err := Start(opts, manager, "gen")
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close(context.Background())
			if err := session.rt.do(t.Context(), func(rt *runtime) error { rt.clock = newManualActivityClock(); return nil }); err != nil {
				t.Fatal(err)
			}
			sub := session.SubscribeEvents(config.DefaultLimits())
			defer sub.Close()
			receipt, err := session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"write"}`)})
			if err != nil {
				t.Fatal(err)
			}
			<-reached
			frame := activityFrame(t, session)
			sub.sub.mu.Lock()
			started := 0
			for _, q := range sub.sub.queue {
				if q.event.Type == "message.started" {
					started++
					if q.event.DurableSeq != nil || q.event.StreamID == "" || q.event.ChunkSeq == nil || *q.event.ChunkSeq != 0 {
						t.Error("started identity is not temporary")
					}
				}
			}
			sub.sub.mu.Unlock()
			if started != 1 {
				t.Error("message.started missing before first HTTP")
			}
			close(release)
			activityWait(t, frame)
			if err := session.rt.do(t.Context(), func(rt *runtime) error {
				if len(rt.modelChunks) != 0 {
					t.Error("terminal stream state retained")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			v := manager.View()
			if requests.Load() != int32(len(tc.statuses)) || runs.Load() != tc.wantRuns || v.Traces[receipt.TraceID].State != tc.wantState || len(v.ModelAttempts) != len(tc.statuses) || len(v.AttemptResults) != len(tc.statuses) {
				t.Fatalf("requests=%d tools=%d attempts=%d terminals=%d trace=%s", requests.Load(), runs.Load(), len(v.ModelAttempts), len(v.AttemptResults), v.Traces[receipt.TraceID].State)
			}
			for id, initial := range v.ModelAttempts {
				if initial.ModelConfigVersion != "fixture-v2" {
					t.Error("attempt configuration version missing")
				}
				outcome := v.AttemptResults[id]
				evidence, ok := v.AttemptDetails[outcome.UsageRef]
				if !ok || evidence.AttemptID != id || outcome.DiagnosticRef != outcome.UsageRef {
					t.Error("terminal evidence reference mismatch")
				} else {
					if outcome.State == "accepted" {
						if len(evidence.Usage) != 1 || !evidence.Usage[0].Snapshot.Usage.InputTotal.Known || evidence.Usage[0].Snapshot.Usage.InputTotal.Value != 12 || evidence.Usage[0].Snapshot.Usage.OutputTotal.Value != 3 {
							t.Error("accepted attempt usage missing or double-counted")
						}
					} else if evidence.FailureReason == "" || evidence.FailureCode == "" {
						t.Error("failed attempt lacks safe reason")
					}
				}
				raw, _ := json.Marshal(v.AttemptResults[id])
				var terminal map[string]any
				_ = json.Unmarshal(raw, &terminal)
				if terminal["usageRef"] == nil || terminal["diagnosticRef"] == nil {
					t.Error("attempt usage/diagnostic references missing")
				}
			}
			if tc.name == "partial_then_success" {
				incomplete, complete := 0, 0
				for _, msg := range v.Messages {
					if msg.Kind != agent.KindAssistant {
						continue
					}
					raw, _ := json.Marshal(msg)
					if msg.Status == agent.StatusIncomplete {
						incomplete++
						if !strings.Contains(string(raw), "failed-prefix") {
							t.Error("partial diagnostic missing")
						}
					}
					if msg.Status == agent.StatusComplete {
						complete++
						if strings.Contains(string(raw), "failed-prefix") || !strings.Contains(string(raw), "done") {
							t.Error("failed attempt concatenated into success")
						}
					}
				}
				if incomplete != 1 || complete != 1 || len(v.Calls) != 0 {
					t.Fatal("partial retry created extra messages or tool calls")
				}
			}
			rawView, _ := json.Marshal(v)
			if strings.Contains(string(rawView), "synthetic-private") {
				t.Error("provider error escaped into durable view")
			}
			if tc.name == "overflow" {
				if !strings.Contains(v.Traces[receipt.TraceID].Error, "no committed replacement projection") {
					t.Error("overflow lacks no-projection diagnostic")
				}
				for _, evidence := range v.AttemptDetails {
					if evidence.FailureReason != "context_overflow" {
						t.Error("overflow reason missing")
					}
				}
			}
			if tc.name == "overflow_unverified" {
				for _, evidence := range v.AttemptDetails {
					if evidence.FailureReason != "invalid_request" {
						t.Error("uncertified overflow code accepted")
					}
				}
			}
			if v.Traces[receipt.TraceID].Usage.TransportRequests != int(requests.Load()) {
				t.Fatal("physical accounting mismatch")
			}
		})
	}
}
