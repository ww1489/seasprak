package llm_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func TestGeminiIncompleteGatewayStreamRejected(t *testing.T) {
	for _, finish := range []string{"", `,"finishReason":"STOP"`} {
		t.Run(fmt.Sprint(finish != ""), func(t *testing.T) {
			body := "data: " + `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{}}}]}}]}` + "\n\n"
			body += "data: " + `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"args":{"arguments":"{\"q\":"}}}]}` + finish + `}]}` + "\n\n"
			url, client, _ := p2ProbeServer(t, p2ProbeReply{true, body})
			cfg := geminiConfig()
			cfg.Endpoint = url
			m := p2FactoryReplayBind(t, cfg, func(c *llm.Catalog) error { return c.RegisterGeminiGenerateContent(client, 1<<20) })
			var observed atomic.Int32
			msg, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, true, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
			p2Code(t, err, product.CodeInvalidArgument)
			if msg != nil || observed.Load() != 1 {
				t.Fatal("incomplete stream accepted or additional request issued")
			}
		})
	}
}

func TestGeminiIDlessCallAndGatewayArgumentFragments(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			frame := func(call string, finish bool) string {
				end := ""
				if finish {
					end = `,"finishReason":"STOP"`
				}
				return `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":` + call + `}]} ` + end + `}]}`
			}
			body := frame(`{"name":"lookup","args":{"q":"ping"}}`, true)
			if stream {
				body = "data: " + frame(`{"name":"lookup","args":{}}`, false) + "\n\n"
				for i, fragment := range []string{`{"q":`, `"pi`, `ng"}`} {
					raw, _ := json.Marshal(map[string]any{"args": map[string]any{"arguments": fragment}})
					body += "data: " + frame(string(raw), i == 2) + "\n\n"
				}
			}
			final := geminiBody("STOP", "pong", false, true)
			if stream {
				final = "data: " + final + "\n\n"
			}
			url, client, requests := p2ProbeServer(t, p2ProbeReply{stream, body}, p2ProbeReply{stream, final})
			cfg := geminiConfig()
			cfg.Endpoint = url
			m := p2FactoryReplayBind(t, cfg, func(c *llm.Catalog) error { return c.RegisterGeminiGenerateContent(client, 1<<20) })
			var observed atomic.Int32
			input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
			msg, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, input)
			p2OK(t, err)
			if len(msg.ContentBlocks) != 1 || msg.ContentBlocks[0].FunctionToolCall == nil {
				t.Fatal("expected exactly one reconstructed call")
			}
			call := msg.ContentBlocks[0].FunctionToolCall
			var args map[string]any
			if call.CallID == "" || call.Name != "lookup" || json.Unmarshal([]byte(call.Arguments), &args) != nil || len(args) != 1 || args["q"] != "ping" {
				t.Fatal("call identity or exact arguments lost")
			}
			// JSON persistence must preserve the assigned ID; input replay must not generate another.
			msg = p2FactoryReplayJSON(t, msg)
			result := &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: call.CallID, Name: call.Name, Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: `{"answer":"pong"}`}}}})}}
			_, err = p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, append(input, msg, result))
			p2OK(t, err)
			p2ProbeRequest(t, requests)
			request := p2ProbeRequest(t, requests)
			raw, _ := json.Marshal(request)
			if observed.Load() != 2 || strings.Count(string(raw), call.CallID) != 2 {
				t.Fatal("call/result did not replay one stable identity with two physical requests")
			}
		})
	}
}
