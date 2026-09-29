package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// Private replay data is meaningful only for its original model binding. A
// digest avoids storing endpoint/account details in message metadata. This is
// compatibility provenance, not authorization or signature verification.
const replaySourcePrefix = "seasprak.replay-source."

func (m *catalogModel) replaySource() string {
	data, _ := json.Marshal([]string{m.config.Provider, m.config.Protocol, m.config.Model, m.config.Version, m.config.Endpoint, m.config.AccountScope})
	digest := sha256.Sum256(data)
	return replaySourcePrefix + hex.EncodeToString(digest[:])
}

func privateReplayBlock(b *schema.ContentBlock) bool {
	if b.Reasoning != nil && b.Reasoning.Signature != "" {
		return true
	}
	// These keys belong to the pinned adapters; opaque/redacted thinking and
	// Gemini thought signatures need replay even when no Reasoning text exists.
	for _, key := range []string{"_eino_ext_agentic_claude_redacted_thinking", "_eino_ext_agentic_gemini_thought_signature"} {
		if _, ok := b.Extra[key]; ok {
			return true
		}
	}
	return false
}

func (m *catalogModel) validatePrivateReplay(b *schema.ContentBlock) error {
	if !privateReplayBlock(b) {
		return nil
	}
	expected := m.replaySource()
	found := false
	for key := range b.Extra {
		if strings.HasPrefix(key, replaySourcePrefix) {
			if key != expected {
				return unsupported("private replay data requires its original model binding")
			}
			found = true
		}
	}
	if !found {
		return unsupported("private replay data has no compatible model provenance")
	}
	return nil
}

func (m *catalogModel) bindPrivateReplay(msg *schema.AgenticMessage) *schema.AgenticMessage {
	if msg == nil {
		return nil
	}
	var result *schema.AgenticMessage
	for i, b := range msg.ContentBlocks {
		if b == nil || !privateReplayBlock(b) {
			continue
		}
		if result == nil {
			copy := *msg
			copy.ContentBlocks = append([]*schema.ContentBlock(nil), msg.ContentBlocks...)
			result = &copy
		}
		block := *b
		block.Extra = maps.Clone(b.Extra)
		if block.Extra == nil {
			block.Extra = make(map[string]any)
		}
		// Use an idempotent key, not a string value that stream concatenation could
		// append repeatedly when signature fragments arrive in several chunks.
		block.Extra[m.replaySource()] = true
		result.ContentBlocks[i] = &block
	}
	if result != nil {
		return result
	}
	return msg
}
