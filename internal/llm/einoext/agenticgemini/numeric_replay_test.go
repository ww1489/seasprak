package agenticgemini

import (
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestNumericReplayConversion(t *testing.T) {
	const text = `{"integer":9007199254740993}`
	call, err := convFunctionToolCall(&schema.FunctionToolCall{Name: "lookup", Arguments: text})
	if err != nil {
		t.Fatal(err)
	}
	result, err := convFunctionToolResult(&schema.FunctionToolResult{Name: "lookup", Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: text}}}})
	if err != nil {
		t.Fatal(err)
	}
	if call.FunctionCall.Args["integer"] != json.Number("9007199254740993") || result.FunctionResponse.Response["integer"] != json.Number("9007199254740993") {
		t.Fatal("conversion lost the integer before entering the provider SDK")
	}
}

func TestNumericReplayJSONBoundaries(t *testing.T) {
	for _, text := range []string{"", "plain result", `"string result"`, `123`, `true`, `[]`, `{"n":`, `{} {}`} {
		t.Run(text, func(t *testing.T) {
			if _, err := convFunctionToolCall(&schema.FunctionToolCall{Name: "lookup", Arguments: text}); err == nil {
				t.Error("invalid or non-object call arguments accepted")
			}
			part, err := convFunctionToolResult(&schema.FunctionToolResult{Name: "lookup", CallID: "numeric-call", Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: text}}}})
			if err != nil {
				t.Fatal(err)
			}
			if part.FunctionResponse.ID != "numeric-call" || part.FunctionResponse.Response["output"] != text {
				t.Error("non-object result did not retain its original text")
			}
		})
	}
	// Preserve existing null semantics instead of introducing object validation.
	for _, text := range []string{`null`, `{}`} {
		if _, err := convFunctionToolCall(&schema.FunctionToolCall{Name: "lookup", Arguments: text}); err != nil {
			t.Fatal(err)
		}
		part, err := convFunctionToolResult(&schema.FunctionToolResult{Name: "lookup", Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: text}}}})
		if err != nil || len(part.FunctionResponse.Response) != 0 {
			t.Fatal("empty object or null result semantics changed")
		}
	}
}
