package llm

import (
	"net/url"
	"strings"

	product "github.com/ww1489/seasprak/internal/errors"
)

func invalid(message string) error { return product.NewError(product.CodeInvalidArgument, message) }
func unsupported(message string) error {
	return product.NewError(product.CodeUnsupportedCapability, message)
}

func supported(c Capability) bool { return c.Status == Declared || c.Status == Verified }

func validateCapability(c Capability, config ModelConfig) error {
	if c.Status != Declared && c.Status != Verified && c.Status != Unsupported {
		return invalid("invalid capability status")
	}
	if supported(c) && (len(c.Evidence) == 0 || strings.TrimSpace(c.Evidence[0]) == "") {
		return invalid("capability evidence is required")
	}
	if c.Status == Verified && (c.AdapterVersion == "" || c.Endpoint != config.Endpoint || c.Model != config.Model || c.ModelVersion == "" || c.ConfigVersion != config.Version) {
		return invalid("verified capability evidence scope does not match configuration")
	}
	return nil
}

func validateConfig(c ModelConfig) error {
	if strings.TrimSpace(c.Provider) == "" || strings.TrimSpace(c.Protocol) == "" || strings.TrimSpace(c.Model) == "" || strings.TrimSpace(c.Version) == "" {
		return invalid("model identity and version are required")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return invalid("endpoint must be an HTTP service URL without authentication, query or fragment")
	}
	if c.AccountScope == "" || (c.NoCredentials && c.CredentialRef != "") || (!c.NoCredentials && strings.TrimSpace(c.CredentialRef) == "") {
		return invalid("account scope and explicit credential policy are required")
	}
	for _, v := range c.Capabilities.Items {
		if err := validateCapability(v, c); err != nil {
			return err
		}
	}
	for level, v := range c.Capabilities.Thinking {
		if thinkingRank(level) < 0 || level == "max" || v.BudgetTokens < 0 {
			return invalid("invalid thinking mapping")
		}
		if err := validateCapability(v.Capability, c); err != nil {
			return err
		}
		if level == "off" && v.BudgetTokens != 0 {
			return invalid("off cannot reserve thinking tokens")
		}
	}
	if c.Capabilities.ContextWindowTokens < 0 || c.Capabilities.MaxOutputTokens < 0 || c.Parameters.MaxOutputTokens < 0 || c.Parameters.ConservativeContextWindow < 0 || c.Parameters.MinAnswerTokens < 0 {
		return invalid("token limits cannot be negative")
	}
	if c.Parameters.PolicyVersion == "" {
		return invalid("option policy version is required")
	}
	return nil
}

var thinkingLevels = [...]string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

func thinkingRank(level string) int {
	for i, v := range thinkingLevels {
		if v == level {
			return i
		}
	}
	return -1
}

// ResolveOptions freezes budget-relevant values. It never reads credentials or
// contacts a provider. Positive thinking choices use verified mappings only.
func ResolveOptions(config ModelConfig, requested RequestedOptions) (EffectiveOptions, error) {
	if err := validateConfig(config); err != nil {
		return EffectiveOptions{}, err
	}
	out := EffectiveOptions{RequestedThinking: requested.Thinking, RequestedCacheIntent: requested.CacheIntent, Version: config.Parameters.PolicyVersion, ImplicitCacheMayApply: true}
	fail := func(err error) (EffectiveOptions, error) { return EffectiveOptions{}, err }
	if requested.ServerSideEffects || requested.StatefulContinuation || requested.ExplicitCacheResource || requested.AutoFailover {
		return fail(unsupported("requested service-side operation is not enabled"))
	}
	if !supported(config.Capabilities.Capability(CapText)) {
		return fail(unsupported("text capability is required"))
	}
	if !supported(config.Capabilities.Capability(CapPhysicalRequestMetering)) {
		return fail(unsupported("physical request metering declaration is required"))
	}
	for _, name := range requested.RequiredCapabilities {
		if !supported(config.Capabilities.Capability(name)) {
			return fail(unsupported("required model capability is unavailable"))
		}
	}
	out.ContextWindowTokens = config.Capabilities.ContextWindowTokens
	if out.ContextWindowTokens == 0 || !supported(config.Capabilities.Capability(CapContextWindow)) {
		out.ContextWindowTokens = config.Parameters.ConservativeContextWindow
		out.ContextWindowConservative = true
	}
	if out.ContextWindowTokens <= 0 {
		return fail(unsupported("context window or trusted conservative limit is required"))
	}
	if config.Capabilities.MaxOutputTokens <= 0 || !supported(config.Capabilities.Capability(CapOutputLimit)) {
		return fail(unsupported("output limit declaration is required"))
	}
	if requested.MaxOutputTokens < 0 {
		return fail(invalid("output limit cannot be negative"))
	}
	out.MaxOutputTokens = requested.MaxOutputTokens
	if out.MaxOutputTokens == 0 {
		out.MaxOutputTokens = config.Parameters.MaxOutputTokens
	}
	if out.MaxOutputTokens == 0 {
		out.MaxOutputTokens = config.Capabilities.MaxOutputTokens
	}
	if out.MaxOutputTokens > config.Capabilities.MaxOutputTokens || out.MaxOutputTokens > out.ContextWindowTokens {
		return fail(invalid("output limit exceeds model limits"))
	}
	level := requested.Thinking
	if level == "" {
		level = config.Parameters.DefaultThinking
	}
	if level != "" {
		rank := thinkingRank(level)
		if rank < 0 {
			return fail(invalid("unknown thinking level"))
		}
		exact := config.Capabilities.Thinking[level]
		chosen := ""
		if level != "max" && exact.Capability.Status == Verified {
			chosen = level
		}
		if chosen == "" && level != "off" && (!requested.ExactThinking || level == "max") {
			for i := rank; i < len(thinkingLevels)-1; i++ {
				if config.Capabilities.Thinking[thinkingLevels[i]].Capability.Status == Verified {
					chosen = thinkingLevels[i]
					break
				}
			}
			if chosen == "" {
				for i := min(rank-1, len(thinkingLevels)-2); i > 0; i-- {
					if config.Capabilities.Thinking[thinkingLevels[i]].Capability.Status == Verified {
						chosen = thinkingLevels[i]
						break
					}
				}
			}
		}
		if chosen == "" {
			return fail(unsupported("requested thinking control cannot be honored"))
		}
		mapping := config.Capabilities.Thinking[chosen]
		out.EffectiveThinking = chosen
		out.NativeThinking = mapping.NativeValue
		out.ThinkingBudgetTokens = mapping.BudgetTokens
		if requested.Thinking == "" {
			out.ThinkingReason = "assembly_default"
		} else if level == "max" {
			out.ThinkingReason = "maximum_verified_level"
		} else if chosen != level {
			out.ThinkingReason = "nearest_verified_level_up_then_down"
		}
	}
	answer := max(1, config.Parameters.MinAnswerTokens)
	if out.MaxOutputTokens < answer || (config.Capabilities.ThinkingSharesOutput && out.ThinkingBudgetTokens > out.MaxOutputTokens-answer) {
		return fail(invalid("output limit cannot accommodate thinking and required answer"))
	}
	out.CacheIntent = requested.CacheIntent
	if out.CacheIntent == "" {
		out.CacheIntent = "short"
	}
	switch out.CacheIntent {
	case "none":
		out.CacheReason = "active_cache_disabled_implicit_cache_not_controlled"
	case "long":
		if config.Capabilities.Capability(CapCacheLong).Status == Verified {
			out.ActiveCache = true
		} else {
			out.CacheIntent = "short"
			out.CacheReason = "long_unverified_fallback_short"
			out.ActiveCache = config.Capabilities.Capability(CapCacheShort).Status == Verified
		}
	case "short":
		out.ActiveCache = config.Capabilities.Capability(CapCacheShort).Status == Verified
	default:
		return fail(invalid("unknown cache intent"))
	}
	if out.CacheIntent == "short" && !out.ActiveCache && out.CacheReason == "" {
		out.CacheReason = "no_verified_active_cache_strategy"
	}
	return out, nil
}
