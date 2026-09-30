package agent

import (
	"encoding/json"

	"github.com/cloudwego/eino/schema"
)

// PublicMessage copies a serializable history message for display. It must never
// replace the original history used by ConvertToLLM: signatures and provider
// extensions are private replay material, not public reasoning.
func PublicMessage(msg AgentMessage) AgentMessage {
	raw, err := json.Marshal(msg)
	if err != nil {
		panic(err)
	}
	var out AgentMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return PublicMessageOwned(out)
}

// PublicMessageOwned projects a message whose complete object graph is owned by
// the caller. It mutates that graph in place and returns it without copying.
// The caller must not reuse the message for private replay after this call.
func PublicMessageOwned(msg AgentMessage) AgentMessage {
	publicStandard(msg.Standard)
	if msg.Custom != nil {
		publicStandard(msg.Custom.Content)
	}
	return msg
}

func publicStandard(msg *schema.AgenticMessage) {
	if msg == nil {
		return
	}
	// Match the finite normalized finish values used by diagnosticPartial.
	finish, _ := msg.Extra["seasprak.finish"].(string)
	msg.Extra = nil
	switch finish {
	case "stop", "tool_calls", "length", "refusal":
		msg.Extra = map[string]any{"seasprak.finish": finish}
	}
	if msg.ResponseMeta != nil {
		// Provider response extensions have no public-display contract. TokenUsage
		// contains only the existing typed numeric counters, not opaque metadata.
		msg.ResponseMeta = &schema.AgenticResponseMeta{TokenUsage: msg.ResponseMeta.TokenUsage}
	}
	for _, block := range msg.ContentBlocks {
		if block == nil {
			continue
		}
		block.Extra = nil
		if block.Reasoning != nil {
			block.Reasoning.Signature = ""
			block.Reasoning.OpenAIExtension = nil
		}
		if block.AssistantGenText != nil {
			block.AssistantGenText.OpenAIExtension = nil
			block.AssistantGenText.ClaudeExtension = nil
			block.AssistantGenText.Extension = nil
		}
		if block.FunctionToolResult != nil {
			for _, content := range block.FunctionToolResult.Content {
				if content != nil {
					content.Extra = nil
				}
			}
		}
	}
}
