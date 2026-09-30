package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/claude"
	"github.com/cloudwego/eino/schema/gemini"
	"github.com/cloudwego/eino/schema/openai"
)

func TestPublicMessageProjection(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "standard", true: "custom"}[custom], func(t *testing.T) {
			call := &schema.FunctionToolCall{CallID: "call-1", Name: "read", Arguments: `{"signature":"business field","extra":{"keep":true}}`}
			result := &schema.FunctionToolResult{CallID: "call-1", Name: "read", Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: "result"}, Extra: map[string]any{"vendor": "private-result"}}}}
			standard := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, Extra: map[string]any{"seasprak.finish": "tool_calls", "vendor": "private-message"}, ResponseMeta: &schema.AgenticResponseMeta{TokenUsage: &schema.TokenUsage{TotalTokens: 7}, Extension: "private-response", OpenAIExtension: &openai.ResponseMetaExtension{ID: "private-openai"}, ClaudeExtension: &claude.ResponseMetaExtension{ID: "private-claude"}, GeminiExtension: &gemini.ResponseMetaExtension{ID: "private-gemini"}}, ContentBlocks: []*schema.ContentBlock{
				{Type: schema.ContentBlockTypeReasoning, Reasoning: &schema.Reasoning{Text: "visible", Signature: "private-signature", OpenAIExtension: &openai.ReasoningExtension{}}, Extra: map[string]any{"arbitrary": "private-block"}},
				schema.NewContentBlock(&schema.Reasoning{}),
				schema.NewContentBlock(&schema.AssistantGenText{Text: "answer", Extension: "private-text"}),
				schema.NewContentBlock(call), schema.NewContentBlock(result),
			}}
			msg := AgentMessage{ID: "fixture", Kind: KindAssistant, Status: StatusComplete, Source: SourceRef{Kind: SourceModel}, Standard: standard}
			if custom {
				msg.Kind = KindCustom
				msg.Standard = nil
				msg.Custom = &CustomMessage{CustomType: "fixture", Content: standard, Display: true}
			}
			before, _ := json.Marshal(msg)
			var owned AgentMessage
			if err := json.Unmarshal(before, &owned); err != nil {
				t.Fatal(err)
			}
			standardOwned := owned.Standard
			if custom {
				standardOwned = owned.Custom.Content
			}
			projected := PublicMessageOwned(owned)
			projectedStandard := projected.Standard
			if custom {
				projectedStandard = projected.Custom.Content
			}
			if projectedStandard != standardOwned {
				t.Fatal("owned projection copied message")
			}
			out := PublicMessage(msg)
			if !reflect.DeepEqual(projected, out) {
				t.Fatal("owned projection differs from copying projection")
			}
			after, _ := json.Marshal(msg)
			if string(before) != string(after) {
				t.Fatal("projection mutated original")
			}
			raw, _ := json.Marshal(out)
			if strings.Contains(string(raw), "private-") {
				t.Fatal("provider fields leaked")
			}
			visible := out.Standard
			if custom {
				visible = out.Custom.Content
			}
			if len(visible.ContentBlocks) != 5 || visible.ContentBlocks[1].Reasoning == nil || visible.ContentBlocks[0].Reasoning.Text != "visible" || visible.ContentBlocks[2].AssistantGenText.Text != "answer" {
				t.Fatal("display blocks or order changed")
			}
			if !reflect.DeepEqual(visible.ContentBlocks[3].FunctionToolCall, call) || visible.ContentBlocks[4].FunctionToolResult.Content[0].Text.Text != "result" {
				t.Fatal("function invocation or output changed")
			}
			if visible.ResponseMeta.TokenUsage.TotalTokens != 7 || visible.Extra["seasprak.finish"] != "tool_calls" {
				t.Fatal("trusted metadata lost")
			}
			if visible.ContentBlocks[0].Reasoning.OpenAIExtension != nil {
				t.Fatal("reasoning extension remained public")
			}
			visible.ContentBlocks[0].Reasoning.Text = "changed"
			visible.ContentBlocks[3].FunctionToolCall.Arguments = "changed"
			visible.ContentBlocks[4].FunctionToolResult.Content[0].Text.Text = "changed"
			visible.ResponseMeta.TokenUsage.TotalTokens = 99
			visible.Extra["seasprak.finish"] = "changed"
			after, _ = json.Marshal(msg)
			if string(before) != string(after) {
				t.Fatal("public projection shares mutable history")
			}
		})
	}
}
func TestPublicMessageFinishAllowlist(t *testing.T) {
	for _, finish := range []any{"stop", "tool_calls", "length", "refusal", "private-unrecognized", map[string]any{"value": "private-untrusted"}} {
		msg := AgentMessage{Standard: &schema.AgenticMessage{Extra: map[string]any{"seasprak.finish": finish}}}
		out := PublicMessage(msg)
		switch finish {
		case "stop", "tool_calls", "length", "refusal":
			if !reflect.DeepEqual(out.Standard.Extra, msg.Standard.Extra) {
				t.Error("normalized finish dropped")
			}
		default:
			if len(out.Standard.Extra) != 0 {
				t.Error("untrusted finish exposed")
			}
		}
	}
}
