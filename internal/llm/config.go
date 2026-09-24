package llm

// ModelConfig identifies a model without embedding secret values.
type ModelConfig struct {
	Provider      string
	Protocol      string
	Model         string
	Endpoint      string
	CredentialRef string
	Version       string
}

// EffectiveOptions is the immutable option set resolved before a request.
type EffectiveOptions struct {
	RequestedThinking string
	EffectiveThinking string
	MaxOutputTokens   int
	CacheIntent       string
	Version           string
}
