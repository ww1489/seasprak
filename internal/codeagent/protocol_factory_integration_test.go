package codeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/llm"
)

type sessionProtocolCredential struct{ endpoint, provider string }

func (c sessionProtocolCredential) Resolve(context.Context, string) (llm.ResolvedCredential, error) {
	return llm.ResolvedCredential{Secret: "synthetic-local-fixture", Provider: c.provider, Endpoint: c.endpoint, AccountScope: "local"}, nil
}

// These wire fixtures enter the actual factory and Session executor. The
// arithmetic is performed only by the registered tool, never by the driver.
func sessionProtocolBody(protocol string, tool bool) string {
	event := func(v any) string { b, _ := json.Marshal(v); return "data: " + string(b) + "\n\n" }
	const args = `{"a":2,"b":3}`
	switch protocol {
	case "openai-chat", "deepseek-chat":
		delta := map[string]any{"role": "assistant", "content": "5"}
		finish := "stop"
		if tool {
			delete(delta, "content")
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "call-calculate", "type": "function", "function": map[string]any{"name": "calculate", "arguments": args}}}
			finish = "tool_calls"
		}
		return event(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}) + "data: [DONE]\n\n"
	case "openai-responses":
		item := map[string]any{"id": "msg-1", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "5", "annotations": []any{}}}}
		if tool {
			item = map[string]any{"id": "fc-1", "type": "function_call", "status": "completed", "call_id": "call-calculate", "name": "calculate", "arguments": args}
		}
		body := ""
		if !tool {
			body += event(map[string]any{"type": "response.output_text.delta", "item_id": "msg-1", "output_index": 0, "content_index": 0, "delta": "5"})
		}
		body += event(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
		body += event(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		return body + event(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp-1", "object": "response", "status": "completed", "output": []any{item}}})
	case "anthropic-messages":
		frame := func(kind string, v map[string]any) string {
			v["type"] = kind
			return "event: " + kind + "\n" + event(v)
		}
		body := frame("message_start", map[string]any{"message": map[string]any{"id": "msg-1", "type": "message", "role": "assistant", "model": "fixture", "content": []any{}}})
		finish := "end_turn"
		if tool {
			finish = "tool_use"
			body += frame("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "redacted_thinking", "data": "synthetic-private-opaque"}})
			body += frame("content_block_stop", map[string]any{"index": 0})
			body += frame("content_block_start", map[string]any{"index": 1, "content_block": map[string]any{"type": "tool_use", "id": "call-calculate", "name": "calculate", "input": map[string]any{}}})
			body += frame("content_block_delta", map[string]any{"index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": args}})
			body += frame("content_block_stop", map[string]any{"index": 1})
		} else {
			body += frame("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": "5"}})
			body += frame("content_block_stop", map[string]any{"index": 0})
		}
		return body + frame("message_delta", map[string]any{"delta": map[string]any{"stop_reason": finish}}) + frame("message_stop", map[string]any{})
	case "gemini-generate-content":
		part := map[string]any{"text": "5"}
		if tool {
			part = map[string]any{"functionCall": map[string]any{"id": "call-calculate", "name": "calculate", "args": map[string]any{"a": 2, "b": 3}}, "thoughtSignature": "c3ludGhldGljLXByaXZhdGUtc2lnbmF0dXJl"}
		}
		return event(map[string]any{"candidates": []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": []any{part}}, "finishReason": "STOP"}}})
	default:
		panic("unexpected fixture protocol")
	}
}

func TestP2ProtocolFactorySessionCalculationAndDiskReplay(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic-messages", "gemini-generate-content", "deepseek-chat"} {
		t.Run(protocol, func(t *testing.T) {
			var requests, runs atomic.Int32
			private := ""
			if protocol == "anthropic-messages" {
				private = "synthetic-private-opaque"
			}
			if protocol == "gemini-generate-content" {
				private = "c3ludGhldGljLXByaXZhdGUtc2lnbmF0dXJl"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "invalid request", 400)
					return
				}
				n := requests.Add(1)
				if n > 3 {
					t.Error("unexpected extra HTTP request")
					http.Error(w, "unexpected request", 400)
					return
				}
				raw, _ := json.Marshal(body)
				if n > 1 {
					if runs.Load() != 1 || !strings.Contains(string(raw), "answer") || !strings.Contains(string(raw), "call-calculate") {
						t.Error("factory replay lost the executed tool result or identity")
					}
					if private != "" && !strings.Contains(string(raw), private) {
						t.Error("factory replay lost required private content")
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, sessionProtocolBody(protocol, n == 1))
			}))
			defer server.Close()
			provider := "fixture"
			if protocol == "anthropic-messages" {
				provider = "anthropic"
			}
			if protocol == "gemini-generate-content" {
				provider = "google"
			}
			catalog := llm.NewCatalog(sessionProtocolCredential{endpoint: server.URL, provider: provider})
			var err error
			switch protocol {
			case "openai-chat":
				err = catalog.RegisterOpenAIChat(server.Client(), 1<<20)
			case "openai-responses":
				err = catalog.RegisterOpenAIResponses(server.Client(), 1<<20)
			case "anthropic-messages":
				err = catalog.RegisterAnthropicMessages(server.Client(), 1<<20)
			case "gemini-generate-content":
				err = catalog.RegisterGeminiGenerateContent(server.Client(), 1<<20)
			case "deepseek-chat":
				err = catalog.RegisterDeepSeekChat(server.Client(), 1<<20)
			}
			if err != nil {
				t.Fatal(err)
			}
			declared := llm.Capability{Status: llm.Declared, Evidence: []string{"local Session integration fixture"}}
			cfg := llm.ModelConfig{Provider: provider, Protocol: protocol, Model: "fixture", Version: "session-v1", Endpoint: server.URL, CredentialRef: "local-fixture", AccountScope: "local", Capabilities: llm.ModelCapabilities{Items: map[llm.CapabilityName]llm.Capability{}, ContextWindowTokens: 8192, MaxOutputTokens: 512}, Parameters: llm.ModelParameters{MaxOutputTokens: 128, PolicyVersion: "fixture"}}
			for _, cap := range []llm.CapabilityName{llm.CapText, llm.CapTextStream, llm.CapTools, llm.CapContextWindow, llm.CapOutputLimit, llm.CapPhysicalRequestMetering} {
				cfg.Capabilities.Items[cap] = declared
			}
			if err := catalog.Register(cfg); err != nil {
				t.Fatal(err)
			}
			model, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
			if err != nil {
				t.Fatal(err)
			}
			def := tools.Definition{Name: "calculate", Version: "1", Schema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`), Execution: tools.ExecutionDescription{Effect: "read", BackendID: "trusted-run"}, Run: func(_ context.Context, raw json.RawMessage) (string, error) {
				var args struct{ A, B int }
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				runs.Add(1)
				if args.A != 2 || args.B != 3 {
					t.Error("tool arguments changed")
				}
				return fmt.Sprintf(`{"answer":%d}`, args.A+args.B), nil
			}}
			opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: model, Tools: []tools.Definition{def}, GenerationFingerprint: "protocol-session-v1"}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"calculate 2+3"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[input.TraceID].State) })
			v := s.rt.manager.View()
			tr := v.Traces[input.TraceID]
			if tr.State != "completed" || requests.Load() != 2 || runs.Load() != 1 || tr.Usage.TransportRequests != 2 || tr.Usage.LogicalModelCalls != 2 || tr.Usage.ToolExecutions != 1 || len(v.Calls) != 1 || len(v.ModelAttempts) != 2 {
				t.Fatalf("protocol execution error=%s state=%s requests=%d tools=%d usage=%+v calls=%d attempts=%d", tr.Error, tr.State, requests.Load(), runs.Load(), tr.Usage, len(v.Calls), len(v.ModelAttempts))
			}
			for id, call := range v.Calls {
				if id == "call-calculate" || call.Call.ProviderCallID != "call-calculate" || !call.Claimed || call.Observation == nil || call.Observation.SideEffect != "none" {
					t.Fatal("product call identity or observation missing")
				}
			}
			for _, result := range v.AttemptResults {
				if result.State != "accepted" {
					t.Fatal("model attempt was not accepted")
				}
			}
			if len(v.AttemptResults) != 2 {
				t.Fatal("terminal attempts missing")
			}
			snap, err := s.Snapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			answerFound := false
			for _, message := range snap.Messages {
				if message.Standard == nil {
					continue
				}
				for _, block := range message.Standard.ContentBlocks {
					if block.AssistantGenText != nil && block.AssistantGenText.Text == "5" {
						answerFound = true
					}
				}
			}
			if !answerFound {
				t.Fatal("accepted public history lacks the calculated answer")
			}
			public, _ := json.Marshal(struct {
				Snapshot Snapshot
				Events   any
			}{snap, v.Events})
			if strings.Contains(string(public), "synthetic-private-") || strings.Contains(string(public), "seasprak.replay-source.") || private != "" && strings.Contains(string(public), private) {
				t.Fatal("public snapshot leaked provider replay material")
			}
			internal, _ := json.Marshal(v.Messages)
			if private != "" && (!strings.Contains(string(internal), private) || !strings.Contains(string(internal), "seasprak.replay-source.")) {
				t.Fatal("internal history lost private provenance")
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close(context.Background()) })
			restored, _ := json.Marshal(reopened.rt.manager.View().Messages)
			if string(restored) != string(internal) || requests.Load() != 2 || runs.Load() != 1 {
				t.Fatal("disk reopen changed history or executed work")
			}
			next, err := reopened.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"repeat the answer without tools"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(reopened.rt.manager.View().Traces[next.TraceID].State) })
			if reopened.rt.manager.View().Traces[next.TraceID].State != "completed" || requests.Load() != 3 || runs.Load() != 1 {
				t.Fatal("explicit next input failed to replay factory history without repeating tools")
			}
		})
	}
}
