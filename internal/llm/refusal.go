package llm

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/openai"
)

// Only explicit refusal prose is retained, never an error body or arbitrary
// provider metadata. Apply exact current-request credential masking after all
// fragments are collected, before publishing any of the reason.
func (c *UsageCollector) chatRefusal(secret string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.chat == nil || !c.chat.complete || !c.chat.refused {
		return ""
	}
	if c.chat.refusalOverflow {
		return "[refusal reason omitted: size limit]"
	}
	text := c.chat.refusal
	c.chat.refusal = ""
	return safeRefusalText(text, secret)
}

func safeRefusalText(text, secret string) string {
	if secret != "" {
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	text = strings.Map(func(r rune) rune {
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, text)
	// Removing control characters must not reconstruct a credential in output.
	if secret != "" {
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	if len(text) > maxRefusalBytes {
		text = text[:maxRefusalBytes]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return text
}

func addChatRefusal(msg *schema.AgenticMessage, reason string) *schema.ContentBlock {
	if reason == "" {
		return nil
	}
	block := schema.NewContentBlock(&schema.AssistantGenText{OpenAIExtension: &openai.AssistantGenTextExtension{Refusal: &openai.OutputRefusal{Reason: reason}}})
	msg.ContentBlocks = append(msg.ContentBlocks, block)
	return block
}

func originalChatFinish(finish string) string {
	switch finish {
	case "stop", "tool_calls", "length", "content_filter", "refusal":
		return finish
	}
	return "unknown"
}

// ResponseDiagnostic is presentation-only evidence, never an authorization or
// retry signal. L2 uses this L1 accessor instead of inspecting provider types.
// Factory refusal extensions already contain bounded, credential-masked text;
// legacy injected models remain trusted inputs, as with their visible content.
func ResponseDiagnostic(msg *schema.AgenticMessage) (refusal, originalFinish string) {
	if msg == nil || msg.Extra["seasprak.finish"] != "refusal" {
		return "", ""
	}
	if finish, ok := msg.Extra["seasprak.original_finish"].(string); ok {
		originalFinish = originalChatFinish(finish)
	}
	for _, block := range msg.ContentBlocks {
		if block == nil || block.AssistantGenText == nil {
			continue
		}
		ext := block.AssistantGenText.OpenAIExtension
		if ext == nil || ext.Refusal == nil {
			continue
		}
		if len(ext.Refusal.Reason) > maxRefusalBytes-len(refusal) {
			return "[refusal reason omitted: size limit]", originalFinish
		}
		refusal += ext.Refusal.Reason
	}
	return refusal, originalFinish
}
