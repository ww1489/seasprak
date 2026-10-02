package agent

import (
	"encoding/json"

	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
)

// ValidateModelResponse is the shared complete-response rule for execution and
// durable replay. Auxiliary callers never accept function tools, even when
// the provider reports a successful finish.
func ValidateModelResponse(msg *schema.AgenticMessage, allowTools bool) error {
	if msg == nil {
		return product.NewError(product.CodeInternal, "empty model response")
	}
	calls := []*schema.FunctionToolCall{}
	for _, block := range msg.ContentBlocks {
		if !validAssistantBlock(block) {
			return product.NewError(product.CodeInvalidArgument, "malformed model content block")
		}
		if block.FunctionToolCall != nil {
			calls = append(calls, block.FunctionToolCall)
		}
	}
	finish, _ := msg.Extra["seasprak.finish"].(string)
	if msg.Role != schema.AgenticRoleTypeAssistant || (finish != "stop" && finish != "tool_calls") || badToolCalls(calls) || !allowTools && len(calls) != 0 {
		return product.NewError(product.CodeInvalidArgument, "incomplete model response")
	}
	return nil
}
func validAssistantBlock(block *schema.ContentBlock) bool {
	if block == nil {
		return false
	}
	populated := 0
	for _, present := range []bool{block.Reasoning != nil, block.AssistantGenText != nil, block.AssistantGenImage != nil, block.AssistantGenAudio != nil, block.AssistantGenVideo != nil, block.FunctionToolCall != nil, block.UserInputText != nil, block.UserInputImage != nil, block.UserInputAudio != nil, block.UserInputVideo != nil, block.UserInputFile != nil, block.FunctionToolResult != nil, block.ToolSearchFunctionToolResult != nil, block.ServerToolCall != nil, block.ServerToolResult != nil, block.MCPToolCall != nil, block.MCPToolResult != nil, block.MCPListToolsResult != nil, block.MCPToolApprovalRequest != nil, block.MCPToolApprovalResponse != nil} {
		if present {
			populated++
		}
	}
	if populated != 1 {
		return false
	}
	switch block.Type {
	case schema.ContentBlockTypeReasoning:
		return block.Reasoning != nil
	case schema.ContentBlockTypeAssistantGenText:
		return block.AssistantGenText != nil
	case schema.ContentBlockTypeAssistantGenImage:
		return block.AssistantGenImage != nil
	case schema.ContentBlockTypeAssistantGenAudio:
		return block.AssistantGenAudio != nil
	case schema.ContentBlockTypeAssistantGenVideo:
		return block.AssistantGenVideo != nil
	case schema.ContentBlockTypeFunctionToolCall:
		return block.FunctionToolCall != nil
	default:
		return false
	}
}
func badToolCalls(calls []*schema.FunctionToolCall) bool {
	seen := map[string]bool{}
	for _, call := range calls {
		if call.CallID == "" || call.Name == "" || seen[call.CallID] || !json.Valid([]byte(call.Arguments)) {
			return true
		}
		seen[call.CallID] = true
	}
	return false
}
