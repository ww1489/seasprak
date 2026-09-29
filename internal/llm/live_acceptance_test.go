package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

// Shared by offline negative controls and live probes; never include provider data in errors.
func acceptanceMessage(msg *schema.AgenticMessage, tool bool) error {
	invalid := func() error { return product.NewError(product.CodeInvalidArgument, "acceptance contract failed") }
	if msg == nil || msg.Role != schema.AgenticRoleTypeAssistant {
		return invalid()
	}
	var text strings.Builder
	var calls []*schema.FunctionToolCall
	for _, b := range msg.ContentBlocks {
		if b == nil {
			return invalid()
		}
		if b.AssistantGenText != nil {
			text.WriteString(b.AssistantGenText.Text)
		}
		if b.FunctionToolCall != nil {
			calls = append(calls, b.FunctionToolCall)
		}
	}
	if tool {
		if msg.Extra["seasprak.finish"] != "tool_calls" || len(calls) != 1 {
			return invalid()
		}
		call := calls[0]
		var args map[string]any
		if call.CallID == "" || call.Name != "lookup" || json.Unmarshal([]byte(call.Arguments), &args) != nil || len(args) != 1 || args["q"] != "ping" {
			return invalid()
		}
	} else if msg.Extra["seasprak.finish"] != "stop" || len(calls) != 0 || strings.TrimSpace(strings.ToLower(text.String())) != "pong" {
		return invalid()
	}
	return nil
}

func acceptanceError(t *testing.T, stage string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	code := product.CodeInternal
	if e, ok := product.AsError(err); ok {
		code = e.Code
	}
	t.Fatalf("stage=%s code=%s (provider details suppressed)", stage, code)
}

func acceptanceInvoke(t *testing.T, m llm.Model, cfg llm.ModelConfig, physical *atomic.Int32, stream bool, input []*schema.AgenticMessage, opts ...model.Option) *schema.AgenticMessage {
	t.Helper()
	before := physical.Load()
	var observed atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	ctx = llm.WithRequestObservation(ctx, llm.RequestIdentity{ModelCallID: "live-probe", AttemptID: "single-request", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error {
		if observed.Add(1) > 1 {
			return product.NewError(product.CodeBudgetExhausted, "probe request budget exhausted")
		}
		return nil
	}))
	usage := &usageObserver{}
	ctx = llm.WithUsageObservation(ctx, usage)
	msg, err := p2ChatInvoke(ctx, m, stream, input, opts...)
	acceptanceError(t, "invoke", err)
	if observed.Load() != 1 || physical.Load()-before != 1 {
		t.Fatal("request accounting mismatch: expected exactly one observed and physical request")
	}
	usage.mu.Lock()
	defer usage.mu.Unlock()
	if len(usage.snapshots) != 1 {
		t.Fatal("expected exactly one usage observation")
	}
	u := usage.snapshots[0].Usage
	for _, check := range []struct {
		cap   llm.CapabilityName
		value llm.UsageValue
	}{{llm.CapUsageInput, u.InputTotal}, {llm.CapUsageOutput, u.OutputTotal}} {
		status := cfg.Capabilities.Capability(check.cap).Status
		if (status == llm.Declared || status == llm.Verified) && !check.value.Known {
			t.Fatal("declared usage measurement missing")
		}
		if check.value.Known && (check.value.Value < 0 || check.value.Source == "") {
			t.Fatal("invalid usage measurement")
		}
		if !check.value.Known && check.value.Value != 0 {
			t.Fatal("absent usage fabricated")
		}
	}
	if msg != nil && msg.ResponseMeta != nil && msg.ResponseMeta.TokenUsage != nil {
		sdk := msg.ResponseMeta.TokenUsage
		if !u.InputTotal.Known && !u.OutputTotal.Known {
			t.Fatal("absent raw usage became SDK usage")
		}
		if u.InputTotal.Known && int64(sdk.PromptTokens) != u.InputTotal.Value || u.OutputTotal.Known && int64(sdk.CompletionTokens) != u.OutputTotal.Value {
			t.Fatal("SDK and observed usage disagree")
		}
	}
	t.Logf("usage_input_present=%t usage_output_present=%t observed=1 physical=1", u.InputTotal.Known, u.OutputTotal.Known)
	return msg
}

func acceptanceConversation(t *testing.T, m llm.Model, cfg llm.ModelConfig, physical *atomic.Int32, stream bool) {
	t.Helper()
	msg := acceptanceInvoke(t, m, cfg, physical, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("Reply with the single word pong, without punctuation.")})
	acceptanceError(t, "text", acceptanceMessage(msg, false))
	tools := model.WithTools([]*schema.ToolInfo{{Name: "lookup", Desc: "A harmless local test function. Call once with q=ping to obtain the answer.", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"q": {Type: schema.String, Required: true}})}})
	input := []*schema.AgenticMessage{schema.UserAgenticMessage("Call lookup exactly once with q set to ping. After receiving the tool result, reply with its answer only, without punctuation. Do not answer before calling the tool.")}
	callMsg := acceptanceInvoke(t, m, cfg, physical, stream, input, tools)
	acceptanceError(t, "tool_call", acceptanceMessage(callMsg, true))
	var call *schema.FunctionToolCall
	for _, b := range callMsg.ContentBlocks {
		if b.FunctionToolCall != nil {
			call = b.FunctionToolCall
		}
	}
	// This synthetic result is the entire tool execution: no file, shell or network side effect.
	result := &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: call.CallID, Name: call.Name, Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: `{"answer":"pong"}`}}}})}}
	answer := acceptanceInvoke(t, m, cfg, physical, stream, append(input, callMsg, result), tools)
	acceptanceError(t, "tool_answer", acceptanceMessage(answer, false))
}

func TestLiveAcceptanceRejectsInvalidHTTPResponses(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, finish, text string
			tools, want        bool
		}{
			{"valid", "stop", "pong", false, true}, {"empty", "stop", "", false, false}, {"wrong_text", "stop", "wrong", false, false}, {"missing_finish", "", "pong", false, false}, {"invalid_tool_arguments", "tool_calls", "", true, false},
		} {
			t.Run(fmt.Sprintf("%t/%s", stream, tc.name), func(t *testing.T) {
				url, client, _ := p2ProbeServer(t, p2ProbeReply{stream, p2ChatBody(stream, tc.finish, tc.text, false, tc.tools, true)})
				cfg := p2ChatConfig()
				cfg.Endpoint = url + "/v1"
				c := llm.NewCatalog(nil)
				p2OK(t, c.RegisterOpenAIChat(client, 1<<20))
				p2OK(t, c.Register(cfg))
				m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
				p2OK(t, err)
				var observed atomic.Int32
				msg, err := p2ChatInvoke(p2ChatContext(t, &observed), m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
				if err == nil {
					err = acceptanceMessage(msg, tc.tools)
				}
				if (err == nil) != tc.want {
					t.Fatal("HTTP acceptance verdict mismatch")
				}
				if observed.Load() != 1 {
					t.Fatal("expected one observed request")
				}
			})
		}
	}
}

func TestLiveAcceptanceOfflineConversation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("%t", stream), func(t *testing.T) {
			first := p2ChatBody(stream, "stop", "pong", false, false, true)
			call := strings.ReplaceAll(p2ChatBody(stream, "tool_calls", "", false, true, true), "中文😀", "ping")
			url, client, requests := p2ProbeServer(t, p2ProbeReply{stream, first}, p2ProbeReply{stream, call}, p2ProbeReply{stream, first})
			var physical atomic.Int32
			transport := client.Transport
			client.Transport = p2RoundTripper(func(r *http.Request) (*http.Response, error) { physical.Add(1); return transport.RoundTrip(r) })
			cfg := p2ChatConfig()
			cfg.Endpoint = url + "/v1"
			c := llm.NewCatalog(nil)
			p2OK(t, c.RegisterOpenAIChat(client, 1<<20))
			p2OK(t, c.Register(cfg))
			m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, err)
			acceptanceConversation(t, m, cfg, &physical, stream)
			p2ProbeRequest(t, requests)
			p2ProbeRequest(t, requests)
			request := p2ProbeRequest(t, requests)
			messages, _ := request["messages"].([]any)
			if len(messages) != 3 {
				t.Fatal("tool replay message count mismatch")
			}
			last, _ := messages[2].(map[string]any)
			if last["role"] != "tool" || last["tool_call_id"] != "call-a" {
				t.Fatal("tool result identity lost")
			}
		})
	}
}
