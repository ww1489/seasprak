package llm

import (
	"strings"
	"testing"
)

func TestSessionPromptCacheKeyIsolation(t *testing.T) {
	base := ResolvedModelConfig{Config: ModelConfig{Provider: "openai", Protocol: "openai-responses", Endpoint: "https://example.invalid/v1", Model: "fixture", Version: "config-v1"}, AccountScope: "account-one", Options: EffectiveOptions{Version: "policy-v1"}}
	ctx := WithSessionCacheScope(t.Context(), "session-one")
	first, err := sessionPromptCacheKey(ctx, base)
	if err != nil || len(first) != 64 || strings.Contains(first, "session") {
		t.Fatalf("key=%q err=%v", first, err)
	}
	again, err := sessionPromptCacheKey(WithSessionCacheScope(t.Context(), "session-one"), base)
	if err != nil || first != again {
		t.Fatal("same session binding did not preserve key")
	}
	other, err := sessionPromptCacheKey(WithSessionCacheScope(ctx, "session-two"), base)
	if err != nil || other == first {
		t.Fatal("different session reused key")
	}
	for _, tc := range []struct {
		name   string
		change func(*ResolvedModelConfig)
	}{
		{"provider", func(c *ResolvedModelConfig) { c.Config.Provider = "other" }},
		{"protocol", func(c *ResolvedModelConfig) { c.Config.Protocol = "other" }},
		{"endpoint", func(c *ResolvedModelConfig) { c.Config.Endpoint = "https://other.invalid" }},
		{"model", func(c *ResolvedModelConfig) { c.Config.Model = "other" }},
		{"account", func(c *ResolvedModelConfig) { c.AccountScope = "other" }},
		{"configuration", func(c *ResolvedModelConfig) { c.Config.Version = "other" }},
		{"policy", func(c *ResolvedModelConfig) { c.Options.Version = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.change(&changed)
			got, err := sessionPromptCacheKey(ctx, changed)
			if err != nil || got == first {
				t.Fatalf("binding change reused key: %v", err)
			}
		})
	}
}
