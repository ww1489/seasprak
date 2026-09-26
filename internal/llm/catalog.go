package llm

import (
	"context"
	"maps"
	"strings"
	"sync"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ModelKey uses separate comparable fields, so delimiters cannot alias identities.
type ModelKey struct{ Provider, Protocol, Model, Version string }

func (c ModelConfig) Key() ModelKey { return ModelKey{c.Provider, c.Protocol, c.Model, c.Version} }

// ResolvedModelConfig is an isolated, secret-free request description. Factories
// must not initiate network requests: Generate/Stream own request initiation.
type ResolvedModelConfig struct {
	Config       ModelConfig
	Options      EffectiveOptions
	AccountScope string
}
type ModelFactory func(context.Context, ResolvedModelConfig) (Model, error)

type factoryRegistration struct {
	build    ModelFactory
	observed bool
}

type catalogEntry struct {
	config   ModelConfig
	injected Model
}

// Catalog stores immutable registrations. It installs no protocol by default.
// Factories and resolvers are trusted code and must support concurrent calls.
type Catalog struct {
	mu        sync.RWMutex
	entries   map[ModelKey]catalogEntry
	factories map[string]factoryRegistration
	resolver  CredentialResolver
}

func NewCatalog(resolver CredentialResolver) *Catalog {
	return &Catalog{entries: make(map[ModelKey]catalogEntry), factories: make(map[string]factoryRegistration), resolver: resolver}
}

// RegisterObservedFactory declares that the factory routes all physical requests
// through NewObservedTransport. Capability evidence is still checked separately.
func (c *Catalog) RegisterObservedFactory(protocol string, factory ModelFactory) error {
	return c.registerFactory(protocol, factory, true)
}

func (c *Catalog) RegisterFactory(protocol string, factory ModelFactory) error {
	return c.registerFactory(protocol, factory, false)
}

func (c *Catalog) registerFactory(protocol string, factory ModelFactory, observed bool) error {
	if strings.TrimSpace(protocol) == "" || factory == nil {
		return invalid("protocol and factory are required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.factories[protocol]; ok {
		return invalid("protocol factory already registered")
	}
	c.factories[protocol] = factoryRegistration{build: factory, observed: observed}
	return nil
}
func (c *Catalog) Register(config ModelConfig) error { return c.register(config, nil) }

// RegisterInjected accepts a trusted existing AgenticModel, but its declarations
// cannot certify themselves. Verified claims require the protocol test path.
// The metering declaration is mandatory, not proof of physical metering.
func (c *Catalog) RegisterInjected(config ModelConfig, model Model) error {
	if model == nil {
		return invalid("injected model is required")
	}
	for _, cap := range config.Capabilities.Items {
		if cap.Status == Verified {
			return invalid("injected model cannot self-certify capabilities")
		}
	}
	for _, level := range config.Capabilities.Thinking {
		if level.Capability.Status == Verified {
			return invalid("injected model cannot self-certify thinking")
		}
	}
	return c.register(config, model)
}
func (c *Catalog) register(config ModelConfig, injected Model) error {
	config = cloneConfig(config)
	if _, err := ResolveOptions(config, RequestedOptions{}); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[config.Key()]; ok {
		return invalid("model configuration already registered")
	}
	c.entries[config.Key()] = catalogEntry{config: config, injected: injected}
	return nil
}
func (c *Catalog) Lookup(key ModelKey) (ModelConfig, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok {
		return ModelConfig{}, invalid("model configuration is not registered")
	}
	return cloneConfig(entry.config), nil
}

// Bind resolves defaults before budgeting, without constructing the model or
// obtaining credentials. Each call rechecks input and resolves fresh credentials.
func (c *Catalog) Bind(key ModelKey, options RequestedOptions) (Model, error) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	registration := c.factories[key.Protocol]
	factory, observed := registration.build, registration.observed
	resolver := c.resolver
	c.mu.RUnlock()
	if !ok {
		return nil, invalid("model configuration is not registered")
	}
	effective, err := ResolveOptions(entry.config, options)
	if err != nil {
		return nil, err
	}
	if entry.injected != nil {
		injected := entry.injected
		observed = UsesObservedTransport(injected)
		factory = func(context.Context, ResolvedModelConfig) (Model, error) { return injected, nil }
	}
	if factory == nil {
		return nil, unsupported("model protocol has no registered factory")
	}
	if !observed && entry.config.Capabilities.Capability(CapPhysicalRequestMetering).Status == Verified {
		return nil, unsupported("verified physical metering requires an observed transport factory")
	}
	return &catalogModel{config: cloneConfig(entry.config), options: effective, factory: factory, resolver: resolver, observed: observed}, nil
}

func cloneConfig(c ModelConfig) ModelConfig {
	c.Capabilities.Items = maps.Clone(c.Capabilities.Items)
	for key, value := range c.Capabilities.Items {
		value.Evidence = append([]string(nil), value.Evidence...)
		c.Capabilities.Items[key] = value
	}
	c.Capabilities.Thinking = maps.Clone(c.Capabilities.Thinking)
	for key, value := range c.Capabilities.Thinking {
		value.Capability.Evidence = append([]string(nil), value.Capability.Evidence...)
		c.Capabilities.Thinking[key] = value
	}
	return c
}

type catalogModel struct {
	config   ModelConfig
	options  EffectiveOptions
	factory  ModelFactory
	resolver CredentialResolver
	observed bool
}

func (m *catalogModel) UsesObservedTransport() bool { return m.observed }

// Configuration and EffectiveOptions expose copies through optional interfaces;
// the model itself continues to implement the unmodified Eino AgenticModel.
func (m *catalogModel) Configuration() ModelConfig         { return cloneConfig(m.config) }
func (m *catalogModel) EffectiveOptions() EffectiveOptions { return m.options }

func (m *catalogModel) prepare(ctx context.Context, in []*schema.AgenticMessage, stream bool, opts []einomodel.Option) (context.Context, Model, []einomodel.Option, func(), error) {
	fail := func(err error) (context.Context, Model, []einomodel.Option, func(), error) {
		return nil, nil, nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if stream && !supported(m.config.Capabilities.Capability(CapTextStream)) {
		return fail(unsupported("streaming capability is unavailable"))
	}
	if err := m.validateInput(in); err != nil {
		return fail(err)
	}
	// Eino intentionally hides implementation-specific setters. Accept only
	// options with a visible common effect rather than forwarding an opaque
	// setter that could enable remote tools, change authentication or budgets.
	for _, opt := range opts {
		v := einomodel.GetCommonOptions(nil, opt)
		if v.Temperature == nil && v.Model == nil && v.TopP == nil && v.Tools == nil && v.DeferredTools == nil && v.ToolSearchTool == nil && v.MaxTokens == nil && v.Stop == nil && v.ToolChoice == nil && v.AgenticToolChoice == nil && v.AllowedToolNames == nil {
			return fail(unsupported("opaque or empty call-time model option is not supported"))
		}
	}
	common := einomodel.GetCommonOptions(nil, opts...)
	if common.Model != nil && *common.Model != m.config.Model {
		return fail(invalid("call-time model replacement is not allowed"))
	}
	if common.MaxTokens != nil && (*common.MaxTokens < max(1, m.config.Parameters.MinAnswerTokens) || *common.MaxTokens > m.options.MaxOutputTokens) {
		return fail(invalid("call-time output limit exceeds resolved options"))
	}
	if len(common.Tools) > 0 && !supported(m.config.Capabilities.Capability(CapTools)) {
		return fail(unsupported("tool capability is unavailable"))
	}
	if common.ToolSearchTool != nil || len(common.DeferredTools) > 0 {
		return fail(unsupported("server-side tool search is not enabled"))
	}
	// Rebuild only common options. Provider-specific options belong in the trusted
	// factory, where resolved thinking/cache/limits cannot be overridden by callers.
	safe := []einomodel.Option{einomodel.WithModel(m.config.Model), einomodel.WithMaxTokens(m.options.MaxOutputTokens)}
	if common.MaxTokens != nil {
		if m.config.Capabilities.ThinkingSharesOutput && m.options.ThinkingBudgetTokens > *common.MaxTokens-max(1, m.config.Parameters.MinAnswerTokens) {
			return fail(invalid("call-time output limit cannot accommodate thinking"))
		}
		safe = append(safe, einomodel.WithMaxTokens(*common.MaxTokens))
	}
	if common.Temperature != nil {
		safe = append(safe, einomodel.WithTemperature(*common.Temperature))
	}
	if common.TopP != nil {
		safe = append(safe, einomodel.WithTopP(*common.TopP))
	}
	if common.Stop != nil {
		safe = append(safe, einomodel.WithStop(append([]string(nil), common.Stop...)))
	}
	if common.Tools != nil {
		safe = append(safe, einomodel.WithTools(common.Tools))
	}
	if common.ToolChoice != nil {
		safe = append(safe, einomodel.WithToolChoice(*common.ToolChoice, common.AllowedToolNames...))
	}
	if common.AgenticToolChoice != nil {
		safe = append(safe, einomodel.WithAgenticToolChoice(common.AgenticToolChoice))
	}
	requestCtx, release, err := requestContext(ctx, m.config, m.resolver)
	if err != nil {
		return fail(err)
	}
	inner, err := m.factory(requestCtx, ResolvedModelConfig{Config: cloneConfig(m.config), Options: m.options, AccountScope: m.config.AccountScope})
	if err != nil {
		release()
		return fail(safeModelError(err))
	}
	if inner == nil {
		release()
		return fail(invalid("model factory returned no model"))
	}
	if err := ctx.Err(); err != nil {
		release()
		return fail(err)
	}
	return requestCtx, inner, safe, release, nil
}
func (m *catalogModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.AgenticMessage, error) {
	requestCtx, inner, safe, release, err := m.prepare(ctx, in, false, opts)
	if err != nil {
		return nil, err
	}
	defer release()
	requestCtx = withFailureObservation(requestCtx)
	result, err := inner.Generate(requestCtx, in, safe...)
	if err != nil {
		return nil, observedModelError(requestCtx, err)
	}
	return result, nil
}
func (m *catalogModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	requestCtx, inner, safe, release, err := m.prepare(ctx, in, true, opts)
	if err != nil {
		return nil, err
	}
	defer release()
	requestCtx = withFailureObservation(requestCtx)
	reader, err := inner.Stream(requestCtx, in, safe...)
	if err != nil {
		if reader != nil {
			reader.Close()
		}
		return nil, observedModelError(requestCtx, err)
	}
	if reader == nil {
		return nil, invalid("model returned no stream")
	}
	return schema.StreamReaderWithConvert(reader, func(msg *schema.AgenticMessage) (*schema.AgenticMessage, error) { return msg, nil }, schema.WithErrWrapper(func(err error) error { return observedModelError(requestCtx, err) })), nil
}

func (m *catalogModel) validateInput(messages []*schema.AgenticMessage) error {
	require := func(name CapabilityName) error {
		if !supported(m.config.Capabilities.Capability(name)) {
			return unsupported("input capability is unavailable")
		}
		return nil
	}
	for _, msg := range messages {
		if msg == nil {
			return invalid("nil input message")
		}
		for _, b := range msg.ContentBlocks {
			if b == nil {
				return invalid("nil input content block")
			}
			if b.ServerToolCall != nil || b.ServerToolResult != nil || b.MCPToolCall != nil || b.MCPToolResult != nil || b.MCPListToolsResult != nil || b.MCPToolApprovalRequest != nil || b.MCPToolApprovalResponse != nil || b.ToolSearchFunctionToolResult != nil {
				return unsupported("server-side tool content is not enabled")
			}
			switch b.Type {
			case schema.ContentBlockTypeUserInputText, schema.ContentBlockTypeAssistantGenText, schema.ContentBlockTypeReasoning, schema.ContentBlockTypeFunctionToolCall, schema.ContentBlockTypeFunctionToolResult, schema.ContentBlockTypeUserInputImage, schema.ContentBlockTypeUserInputAudio, schema.ContentBlockTypeUserInputVideo, schema.ContentBlockTypeUserInputFile, schema.ContentBlockTypeAssistantGenImage, schema.ContentBlockTypeAssistantGenAudio, schema.ContentBlockTypeAssistantGenVideo:
			default:
				return unsupported("input content type is unavailable")
			}
			for _, check := range []struct {
				present bool
				name    CapabilityName
			}{
				{b.UserInputImage != nil || b.AssistantGenImage != nil || b.Type == schema.ContentBlockTypeUserInputImage || b.Type == schema.ContentBlockTypeAssistantGenImage, CapInputImage},
				{b.UserInputAudio != nil || b.AssistantGenAudio != nil || b.Type == schema.ContentBlockTypeUserInputAudio || b.Type == schema.ContentBlockTypeAssistantGenAudio, CapInputAudio},
				{b.UserInputVideo != nil || b.AssistantGenVideo != nil || b.Type == schema.ContentBlockTypeUserInputVideo || b.Type == schema.ContentBlockTypeAssistantGenVideo, CapInputVideo},
				{b.UserInputFile != nil || b.Type == schema.ContentBlockTypeUserInputFile, CapInputFile},
				{b.FunctionToolCall != nil || b.FunctionToolResult != nil || b.Type == schema.ContentBlockTypeFunctionToolCall || b.Type == schema.ContentBlockTypeFunctionToolResult, CapTools},
			} {
				if check.present {
					if err := require(check.name); err != nil {
						return err
					}
				}
			}
			if b.FunctionToolResult != nil {
				for _, part := range b.FunctionToolResult.Content {
					if part == nil {
						return invalid("nil tool result content")
					}
					switch part.Type {
					case schema.FunctionToolResultContentBlockTypeText, schema.FunctionToolResultContentBlockTypeImage, schema.FunctionToolResultContentBlockTypeAudio, schema.FunctionToolResultContentBlockTypeVideo, schema.FunctionToolResultContentBlockTypeFile:
					default:
						return unsupported("tool result content type is unavailable")
					}
					for _, check := range []struct {
						present bool
						name    CapabilityName
					}{{part.Image != nil || part.Type == schema.FunctionToolResultContentBlockTypeImage, CapInputImage}, {part.Audio != nil || part.Type == schema.FunctionToolResultContentBlockTypeAudio, CapInputAudio}, {part.Video != nil || part.Type == schema.FunctionToolResultContentBlockTypeVideo, CapInputVideo}, {part.File != nil || part.Type == schema.FunctionToolResultContentBlockTypeFile, CapInputFile}} {
						if check.present {
							if err := require(check.name); err != nil {
								return err
							}
						}
					}
				}
			}
		}
	}
	return nil
}
