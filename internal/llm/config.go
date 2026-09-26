package llm

// ModelConfig identifies a model without embedding secret values. AccountScope is
// a non-secret, application-assigned account identity; rotation must preserve it.
type ModelConfig struct {
	Provider      string
	Protocol      string
	Model         string
	Endpoint      string
	CredentialRef string
	Version       string
	AccountScope  string
	NoCredentials bool
	Capabilities  ModelCapabilities
	Parameters    ModelParameters
}

type CapabilityStatus string

const (
	Verified    CapabilityStatus = "verified"
	Declared    CapabilityStatus = "declared"
	Unsupported CapabilityStatus = "unsupported"
)

type CapabilityName string

const (
	CapText                    CapabilityName = "text"
	CapTextStream              CapabilityName = "text_stream"
	CapTools                   CapabilityName = "tools"
	CapMultipleTools           CapabilityName = "multiple_tools"
	CapContextWindow           CapabilityName = "context_window"
	CapContextOverflow         CapabilityName = "context_overflow"
	CapOutputLimit             CapabilityName = "output_limit"
	CapInputImage              CapabilityName = "input_image"
	CapInputAudio              CapabilityName = "input_audio"
	CapInputVideo              CapabilityName = "input_video"
	CapInputFile               CapabilityName = "input_file"
	CapOutputImage             CapabilityName = "output_image"
	CapOutputAudio             CapabilityName = "output_audio"
	CapOutputVideo             CapabilityName = "output_video"
	CapPublicReasoning         CapabilityName = "public_reasoning"
	CapUsageInput              CapabilityName = "usage_input"
	CapUsageOutput             CapabilityName = "usage_output"
	CapUsageUncached           CapabilityName = "usage_uncached"
	CapUsageCacheRead          CapabilityName = "usage_cache_read"
	CapUsageCacheWrite         CapabilityName = "usage_cache_write"
	CapUsageReasoning          CapabilityName = "usage_reasoning"
	CapCacheImplicit           CapabilityName = "cache_implicit"
	CapCacheShort              CapabilityName = "cache_short"
	CapCacheLong               CapabilityName = "cache_long"
	CapCacheResource           CapabilityName = "cache_resource"
	CapStatefulContinuation    CapabilityName = "stateful_continuation"
	CapStructuredOutput        CapabilityName = "structured_output"
	CapServerTools             CapabilityName = "server_tools"
	CapPhysicalRequestMetering CapabilityName = "physical_request_metering"
)

// Capability is a declaration, not a certificate issued by this SDK. Verified
// entries require scoped evidence from the trusted protocol registration path.
type Capability struct {
	Status         CapabilityStatus
	AdapterVersion string
	Endpoint       string
	Model          string
	ModelVersion   string
	ConfigVersion  string
	Evidence       []string
}

type ThinkingSupport struct {
	Capability   Capability
	NativeValue  string
	BudgetTokens int
}

type ModelCapabilities struct {
	Items                map[CapabilityName]Capability
	Thinking             map[string]ThinkingSupport
	ContextWindowTokens  int
	MaxOutputTokens      int
	ThinkingSharesOutput bool
}

// Capability returns an isolated record. An absent capability is unsupported.
func (c ModelCapabilities) Capability(name CapabilityName) Capability {
	v, ok := c.Items[name]
	if !ok {
		return Capability{Status: Unsupported}
	}
	v.Evidence = append([]string(nil), v.Evidence...)
	return v
}

// ModelParameters contains typed defaults only, never arbitrary provider fields
// or authentication headers. Conservative limits are supplied by trusted code.
type ModelParameters struct {
	DefaultThinking           string
	MaxOutputTokens           int
	ConservativeContextWindow int
	MinAnswerTokens           int
	PolicyVersion             string
}

// EffectiveOptions is a value-only snapshot resolved before budgeting a request.
// Existing fields retain their names and types. CacheIntent is effective intent.
type EffectiveOptions struct {
	RequestedThinking         string
	EffectiveThinking         string
	MaxOutputTokens           int
	CacheIntent               string
	Version                   string
	RequestedCacheIntent      string
	ThinkingReason            string
	CacheReason               string
	NativeThinking            string
	ThinkingBudgetTokens      int
	ContextWindowTokens       int
	ContextWindowConservative bool
	ActiveCache               bool
	ImplicitCacheMayApply     bool
}

type RequestedOptions struct {
	Thinking              string
	ExactThinking         bool
	MaxOutputTokens       int
	CacheIntent           string
	RequiredCapabilities  []CapabilityName
	ServerSideEffects     bool
	StatefulContinuation  bool
	ExplicitCacheResource bool
	AutoFailover          bool
}
