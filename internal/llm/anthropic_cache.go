package llm

import (
	"maps"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/llm/einoext/agenticclaude"
)

// This key is owned by the pinned private adapter. Remove inherited markers
// before applying the resolved policy, including when active caching is off.
const anthropicCacheTTLKey = "_agenticclaude_cache_control_ttl"

// anthropicCacheRequest decorates only request copies. At most three explicit
// breakpoints share one TTL; automatic top-level caching remains disabled.
func anthropicCacheRequest(in []*schema.AgenticMessage, opts []model.Option, effective EffectiveOptions) ([]*schema.AgenticMessage, []model.Option, error) {
	var control *anthropic.CacheControlEphemeralParam
	if effective.ActiveCache {
		var err error
		control, err = anthropicCache(effective.CacheIntent)
		if err != nil {
			return nil, nil, err
		}
	}
	messages := make([]*schema.AgenticMessage, len(in))
	systemMessage, systemBlock, userMessage, userBlock := -1, -1, -1, -1
	leadingSystem := true
	for i, msg := range in {
		if msg == nil {
			return nil, nil, invalid("nil Anthropic input message")
		}
		copied := *msg
		copied.ContentBlocks = make([]*schema.ContentBlock, len(msg.ContentBlocks))
		if msg.Role != schema.AgenticRoleTypeSystem {
			leadingSystem = false
		}
		for j, block := range msg.ContentBlocks {
			if block == nil {
				return nil, nil, invalid("nil Anthropic input block")
			}
			b := *block
			b.Extra = maps.Clone(block.Extra)
			delete(b.Extra, anthropicCacheTTLKey)
			copied.ContentBlocks[j] = &b
			if leadingSystem && b.Type == schema.ContentBlockTypeUserInputText && b.UserInputText != nil && b.UserInputText.Text != "" {
				systemMessage, systemBlock = i, j
			}
			if msg.Role == schema.AgenticRoleTypeUser && anthropicCacheableUserBlock(&b) {
				userMessage, userBlock = i, j
			}
		}
		messages[i] = &copied
	}
	if control != nil {
		if systemMessage >= 0 {
			msg := messages[systemMessage]
			msg.ContentBlocks[systemBlock] = agenticclaude.SetContentBlockCacheControl(msg.ContentBlocks[systemBlock], control)
		}
		if userMessage >= 0 {
			msg := messages[userMessage]
			msg.ContentBlocks[userBlock] = agenticclaude.SetContentBlockCacheControl(msg.ContentBlocks[userBlock], control)
		}
	}
	options := append([]model.Option(nil), opts...)
	common := model.GetCommonOptions(nil, opts...)
	if common.Tools != nil {
		tools := make([]*schema.ToolInfo, len(common.Tools))
		last := -1
		for i, tool := range common.Tools {
			if tool == nil {
				return nil, nil, invalid("nil Anthropic tool")
			}
			copied := *tool
			copied.Extra = maps.Clone(tool.Extra)
			delete(copied.Extra, anthropicCacheTTLKey)
			tools[i] = &copied
			last = i
		}
		if control != nil && last >= 0 {
			tools[last] = agenticclaude.SetToolInfoCacheControl(tools[last], control)
		}
		options = append(options, model.WithTools(tools))
	}
	return messages, options, nil
}

func anthropicCacheableUserBlock(block *schema.ContentBlock) bool {
	switch block.Type {
	case schema.ContentBlockTypeUserInputText:
		return block.UserInputText != nil && block.UserInputText.Text != ""
	case schema.ContentBlockTypeUserInputImage:
		return block.UserInputImage != nil
	case schema.ContentBlockTypeUserInputFile:
		return block.UserInputFile != nil
	case schema.ContentBlockTypeFunctionToolResult:
		return block.FunctionToolResult != nil
	default:
		return false
	}
}
