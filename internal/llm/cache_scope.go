package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type sessionCacheScopeKey struct{}

// WithSessionCacheScope is used by the trusted execution adapter, never by
// model arguments. A scope is stable across turns and attempts in one session.
// It partitions cache routing; it does not confer access to cached data.
func WithSessionCacheScope(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionCacheScopeKey{}, sessionID)
}

func sessionPromptCacheKey(ctx context.Context, config ResolvedModelConfig) (string, error) {
	sessionID, _ := ctx.Value(sessionCacheScopeKey{}).(string)
	if sessionID == "" {
		return "", invalid("active prompt caching requires a trusted session scope")
	}
	// Hash a structured tuple so neither secrets nor raw session/account IDs
	// are sent as the routing key, and component boundaries cannot collide.
	data, _ := json.Marshal([]string{sessionID, config.Config.Provider, config.Config.Protocol, config.Config.Endpoint, config.AccountScope, config.Config.Model, config.Config.Version, config.Options.Version})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
