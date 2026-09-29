package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func p2FactoryReplayInvoke(ctx context.Context, m llm.Model, stream bool, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	if !stream {
		return m.Generate(ctx, input, opts...)
	}
	r, err := m.Stream(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var chunks []*schema.AgenticMessage
	for {
		chunk, err := r.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	return schema.ConcatAgenticMessages(chunks)
}

func p2FactoryReplayBind(t *testing.T, cfg llm.ModelConfig, register func(*llm.Catalog) error) llm.Model {
	t.Helper()
	catalog := llm.NewCatalog(p2Resolver(func(context.Context, string) (llm.ResolvedCredential, error) {
		return llm.ResolvedCredential{Secret: "synthetic-replay-key", Provider: cfg.Provider, Endpoint: cfg.Endpoint, AccountScope: cfg.AccountScope}, nil
	}))
	p2OK(t, register(catalog))
	p2OK(t, catalog.Register(cfg))
	m, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	return m
}

func p2FactoryReplayJSON(t *testing.T, msg *schema.AgenticMessage) *schema.AgenticMessage {
	t.Helper()
	data, err := json.Marshal(msg)
	p2OK(t, err)
	var restored schema.AgenticMessage
	p2OK(t, json.Unmarshal(data, &restored))
	return &restored
}

func TestP2FactoryGeminiToolReplay(t *testing.T) {
	const signature = "c3ludGhldGljLXNpZ25hdHVyZQ=="
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			first := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"thoughtSignature":"` + signature + `","functionCall":{"id":"provider-a","name":"lookup","args":{"q":"a"}}},{"functionCall":{"id":"provider-b","name":"lookup","args":{"q":"b"}}}]},"finishReason":"STOP"}]}`
			second := geminiBody("STOP", "done", false, true)
			if stream {
				first = "data: " + `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"thoughtSignature":"` + signature + `","functionCall":{"id":"provider-a","name":"lookup","args":{"q":"a"}}}]}}]}` + "\n\ndata: " + `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"provider-b","name":"lookup","args":{"q":"b"}}}]},"finishReason":"STOP"}]}` + "\n\n"
				second = "data: " + second + "\n\n"
			}
			url, client, requests := p2ProbeServer(t, p2ProbeReply{stream, first}, p2ProbeReply{stream, second})
			cfg := geminiConfig()
			cfg.Endpoint = url
			cfg.Capabilities.Items[llm.CapMultipleTools] = cfg.Capabilities.Items[llm.CapTools]
			m := p2FactoryReplayBind(t, cfg, func(c *llm.Catalog) error { return c.RegisterGeminiGenerateContent(client, 1<<20) })
			var observed atomic.Int32
			ctx := p2ChatContext(t, &observed)
			tools := model.WithTools([]*schema.ToolInfo{{Name: "lookup", Desc: "lookup", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"q": {Type: schema.String}})}})
			input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
			msg, err := p2FactoryReplayInvoke(ctx, m, stream, input, tools)
			p2OK(t, err)
			if msg.Extra["seasprak.finish"] != "tool_calls" {
				t.Error("function response finish was not tool_calls")
			}
			msg = p2FactoryReplayJSON(t, msg)
			if len(msg.ContentBlocks) != 2 {
				t.Fatal("independent function calls were lost")
			}
			results := &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser}
			for i, id := range []string{"provider-a", "provider-b"} {
				call := msg.ContentBlocks[i].FunctionToolCall
				if call == nil || call.CallID != id || call.Name != "lookup" || call.Arguments != fmt.Sprintf(`{"q":"%s"}`, string(rune('a'+i))) {
					t.Fatal("provider identity or arguments changed")
				}
				results.ContentBlocks = append(results.ContentBlocks, schema.NewContentBlock(&schema.FunctionToolResult{CallID: id, Name: "lookup", Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: `{"ok":true}`}}}}))
			}
			_, err = p2FactoryReplayInvoke(ctx, m, stream, append(input, msg, results), tools)
			p2OK(t, err)
			if observed.Load() != 2 {
				t.Error("observed physical request count must equal two")
			}
			p2ProbeRequest(t, requests)
			request := p2ProbeRequest(t, requests)
			contents, _ := request["contents"].([]any)
			counts := map[string]int{}
			for _, raw := range contents {
				content, _ := raw.(map[string]any)
				parts, _ := content["parts"].([]any)
				for _, rawPart := range parts {
					part, _ := rawPart.(map[string]any)
					for _, field := range []string{"functionCall", "functionResponse"} {
						if value, ok := part[field].(map[string]any); ok {
							id, _ := value["id"].(string)
							counts[field+":"+id]++
							if value["name"] != "lookup" {
								t.Error("replayed function name changed")
							}
							if field == "functionCall" && id == "provider-a" && part["thoughtSignature"] != signature {
								t.Error("JSON-restored thought signature was not replayed")
							}
						}
					}
				}
			}
			if len(counts) != 4 {
				t.Error("replay introduced an unexpected function identity")
			}
			for _, field := range []string{"functionCall", "functionResponse"} {
				for _, id := range []string{"provider-a", "provider-b"} {
					if counts[field+":"+id] != 1 {
						t.Error("replay must preserve each call/result identity exactly once")
					}
				}
			}
		})
	}
}

func TestP2FactoryGeminiMissingIdentity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			body := geminiBody("STOP", "", true, true)
			if stream {
				body = "data: " + body + "\n\n"
			}
			url, client, _ := p2ProbeServer(t, p2ProbeReply{stream, body})
			cfg := geminiConfig()
			cfg.Endpoint = url
			m := p2FactoryReplayBind(t, cfg, func(c *llm.Catalog) error { return c.RegisterGeminiGenerateContent(client, 1<<20) })
			var observed atomic.Int32
			msg, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
			p2OK(t, err)
			if msg == nil || len(msg.ContentBlocks) != 1 || msg.ContentBlocks[0].FunctionToolCall == nil || msg.ContentBlocks[0].FunctionToolCall.CallID == "" || observed.Load() != 1 {
				t.Error("missing provider ID must receive a local identity after exactly one physical request")
			}
		})
	}
}

func TestP2FactoryGeminiRejectsIncompleteHistory(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"call_id", "call_arguments", "result_id", "call_payload", "result_payload"} {
			t.Run(fmt.Sprintf("stream_%t/%s", stream, kind), func(t *testing.T) {
				url, client, _ := p2ProbeServer(t)
				cfg := geminiConfig()
				cfg.Endpoint = url
				m := p2FactoryReplayBind(t, cfg, func(c *llm.Catalog) error { return c.RegisterGeminiGenerateContent(client, 1<<20) })
				block := schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider-a", Name: "lookup", Arguments: `{}`})
				role := schema.AgenticRoleTypeAssistant
				switch kind {
				case "call_id":
					block.FunctionToolCall.CallID = ""
				case "call_arguments":
					block.FunctionToolCall.Arguments = `{`
				case "result_id":
					role = schema.AgenticRoleTypeUser
					block = schema.NewContentBlock(&schema.FunctionToolResult{Name: "lookup"})
				case "call_payload":
					block.FunctionToolCall = nil
				case "result_payload":
					role = schema.AgenticRoleTypeUser
					block = &schema.ContentBlock{Type: schema.ContentBlockTypeFunctionToolResult}
				}
				var observed atomic.Int32
				msg, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, []*schema.AgenticMessage{{Role: role, ContentBlocks: []*schema.ContentBlock{block}}})
				p2Code(t, err, product.CodeInvalidArgument)
				if msg != nil || observed.Load() != 0 {
					t.Error("incomplete history must fail before any request")
				}
			})
		}
	}
}

func TestP2FactoryClaudeRedactedReplay(t *testing.T) {
	const opaque = "synthetic-redacted-replay-data"
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			blocks := []string{`{"type":"thinking","thinking":"summary","signature":"synthetic-signature"}`, `{"type":"redacted_thinking","data":"` + opaque + `"}`, `{"type":"text","text":"done"}`}
			body := `{"id":"msg_probe","type":"message","role":"assistant","model":"probe","content":[` + strings.Join(blocks, ",") + `],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":3}}`
			second := anthropicBody("end_turn", "done", false, true)
			if stream {
				body = "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_probe","type":"message","role":"assistant","model":"probe","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n"
				for i, b := range blocks {
					body += fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":%s}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i, b, i)
				}
				body += "event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				second = anthropicStreamBody("end_turn", "done", false, true)
			}
			url, client, requests := p2ProbeServer(t, p2ProbeReply{stream, body}, p2ProbeReply{stream, second})
			cfg := anthropicConfig()
			cfg.Endpoint = url
			m := p2FactoryReplayBind(t, cfg, func(c *llm.Catalog) error { return c.RegisterAnthropicMessages(client, 1<<20) })
			var observed atomic.Int32
			usage := &usageObserver{}
			ctx := llm.WithUsageObservation(p2ChatContext(t, &observed), usage)
			input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
			msg, err := p2FactoryReplayInvoke(ctx, m, stream, input)
			p2OK(t, err)
			msg = p2FactoryReplayJSON(t, msg)
			if len(msg.ContentBlocks) != 3 {
				t.Fatal("redacted or ordinary thinking was lost in JSON round trip")
			}
			if len(usage.snapshots) != 1 {
				t.Fatal("expected one public usage snapshot before replay")
			}
			refusal, finish := llm.ResponseDiagnostic(msg)
			diagnostic, err := json.Marshal([]any{msg.Extra, refusal, finish, usage.snapshots})
			p2OK(t, err)
			if strings.Contains(string(diagnostic), opaque) || strings.Contains(string(diagnostic), "synthetic-signature") {
				t.Fatal("opaque reasoning escaped public diagnostics")
			}
			_, err = p2FactoryReplayInvoke(ctx, m, stream, append(input, msg, schema.UserAgenticMessage("continue")))
			p2OK(t, err)
			if observed.Load() != 2 {
				t.Error("observed physical request count must equal two")
			}
			p2ProbeRequest(t, requests)
			request := p2ProbeRequest(t, requests)
			messages, _ := request["messages"].([]any)
			count := 0
			for _, raw := range messages {
				message, _ := raw.(map[string]any)
				if message["role"] != "assistant" {
					continue
				}
				count++
				content, _ := message["content"].([]any)
				if len(content) != 3 {
					t.Fatal("replayed block count changed")
				}
				for i, rawBlock := range content {
					b, _ := rawBlock.(map[string]any)
					switch i {
					case 0:
						if b["type"] != "thinking" || b["thinking"] != "summary" || b["signature"] != "synthetic-signature" {
							t.Error("ordinary thinking regressed")
						}
					case 1:
						if b["type"] != "redacted_thinking" || b["data"] != opaque {
							t.Error("redacted thinking was not preserved")
						}
					case 2:
						if b["type"] != "text" || b["text"] != "done" {
							t.Error("text or block order changed")
						}
					}
				}
			}
			if count != 1 {
				t.Error("expected one replayed assistant message")
			}
		})
	}
}
