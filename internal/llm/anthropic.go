package llm

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/cloudwego/eino-ext/components/model/agenticclaude"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/claude"
	product "github.com/ww1489/seasprak/internal/errors"
)

// RegisterAnthropicMessages installs the direct Anthropic Messages adapter.
// Bedrock, Vertex, server-side tools and implicit credential loading are kept
// outside this product factory until each has an independent capability proof.
func (c *Catalog) RegisterAnthropicMessages(client *http.Client, maxResponseBytes int) error {
	if maxResponseBytes <= 0 {
		return invalid("positive Anthropic response collection limit is required")
	}
	copied := observedClient(client, "anthropic-messages", maxResponseBytes)
	return c.RegisterObservedFactory("anthropic-messages", func(ctx context.Context, r ResolvedModelConfig) (Model, error) {
		if r.Config.Provider != "anthropic" {
			return nil, unsupported("Anthropic provider route is unavailable")
		}
		if r.Config.NoCredentials {
			return nil, product.NewError(product.CodeUnauthenticated, "Anthropic requires an explicit request credential")
		}
		auth, ok := RequestCredential(ctx)
		if !ok || auth.Secret == "" {
			return nil, product.NewError(product.CodeUnauthenticated, "Anthropic request credential is unavailable")
		}

		config := &agenticclaude.Config{
			APIKey:     auth.Secret,
			BaseURL:    r.Config.Endpoint,
			HTTPClient: &copied,
			Model:      r.Config.Model,
			MaxTokens:  r.Options.MaxOutputTokens,
		}
		if r.Options.EffectiveThinking != "" {
			thinking, err := anthropicThinking(r.Options)
			if err != nil {
				return nil, err
			}
			config.Thinking = thinking
		}
		if r.Options.ActiveCache {
			cache, err := anthropicCache(r.Options.CacheIntent)
			if err != nil {
				return nil, err
			}
			config.CacheControl = cache
		}
		inner, err := agenticclaude.New(ctx, config)
		if err != nil {
			return nil, err
		}
		return &anthropicMessagesModel{inner: inner, options: r.Options}, nil
	})
}

func anthropicThinking(options EffectiveOptions) (*anthropic.ThinkingConfigParamUnion, error) {
	switch options.NativeThinking {
	case "none":
		value := anthropic.ThinkingConfigParamUnion{OfDisabled: &anthropic.ThinkingConfigDisabledParam{}}
		return &value, nil
	case "enabled":
		// anthropic-sdk-go v1.75.0 ThinkingConfigEnabledParam requires
		// at least 1024 tokens and strictly less than the answer/thinking cap.
		if options.ThinkingBudgetTokens < 1024 || options.ThinkingBudgetTokens >= options.MaxOutputTokens {
			return nil, invalid("Anthropic thinking budget must be at least 1024 and less than max output tokens")
		}
		value := anthropic.ThinkingConfigParamUnion{OfEnabled: &anthropic.ThinkingConfigEnabledParam{BudgetTokens: int64(options.ThinkingBudgetTokens)}}
		return &value, nil
	default:
		return nil, unsupported("unsupported Anthropic thinking mapping")
	}
}

func anthropicCache(intent string) (*anthropic.CacheControlEphemeralParam, error) {
	cache := anthropic.NewCacheControlEphemeralParam()
	switch intent {
	case "short":
		cache.TTL = anthropic.CacheControlEphemeralTTLTTL5m
	case "long":
		cache.TTL = anthropic.CacheControlEphemeralTTLTTL1h
	default:
		return nil, unsupported("unsupported Anthropic cache retention")
	}
	return &cache, nil
}

type anthropicMessagesModel struct {
	inner   Model
	options EffectiveOptions
}

func (m *anthropicMessagesModel) validateThinking(opts []model.Option) error {
	if m.options.EffectiveThinking == "" {
		return nil
	}
	options := m.options
	if maxTokens := model.GetCommonOptions(nil, opts...).MaxTokens; maxTokens != nil {
		options.MaxOutputTokens = *maxTokens
	}
	_, err := anthropicThinking(options)
	return err
}

func (*anthropicMessagesModel) UsesObservedTransport() bool { return true }

func anthropicFinish(msg *schema.AgenticMessage) (string, bool) {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.ClaudeExtension == nil {
		return "", false
	}
	finish := msg.ResponseMeta.ClaudeExtension.StopReason
	return finish, finish != ""
}

func validFunctionToolCalls(msg *schema.AgenticMessage) (seen bool, valid bool) {
	valid = true
	if msg == nil {
		return false, true
	}
	for _, block := range msg.ContentBlocks {
		if block == nil || block.FunctionToolCall == nil {
			continue
		}
		seen = true
		call := block.FunctionToolCall
		if call.CallID == "" || call.Name == "" || !json.Valid([]byte(call.Arguments)) {
			valid = false
		}
	}
	return seen, valid
}

func normalizeAnthropicFinish(msg *schema.AgenticMessage) (string, string, error) {
	raw, ok := anthropicFinish(msg)
	if !ok {
		return "", "", invalid("missing Anthropic terminal stop reason")
	}
	seen, valid := validFunctionToolCalls(msg)
	if !valid {
		return "", raw, invalid("Anthropic function tool call is incomplete")
	}
	switch raw {
	case "end_turn", "stop_sequence":
		return "stop", raw, nil
	case "tool_use":
		if !seen {
			return "", raw, invalid("Anthropic tool_use response has no function tool call")
		}
		return "tool_calls", raw, nil
	case "max_tokens":
		return "length", raw, nil
	case "refusal":
		return "refusal", raw, nil
	default:
		return "", raw, invalid("unsupported Anthropic stop reason")
	}
}

func (m *anthropicMessagesModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	if err := m.validateThinking(opts); err != nil {
		return nil, err
	}
	capture := &usageCapture{}
	requestCtx := withUsageCapture(ctx, capture)
	msg, err := m.inner.Generate(requestCtx, in, opts...)
	if err != nil {
		return nil, safeModelError(err)
	}
	finish, original, err := normalizeAnthropicFinish(msg)
	if err != nil {
		return nil, err
	}
	applyCollectedUsage(msg, capture)
	return addNormalizedFinish(msg, finish, original), nil
}

func (m *anthropicMessagesModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	if err := m.validateThinking(opts); err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithCancel(ctx)
	capture := &usageCapture{}
	requestCtx = withUsageCapture(requestCtx, capture)
	inner, err := m.inner.Stream(requestCtx, in, opts...)
	if err != nil {
		cancel()
		return nil, safeModelError(err)
	}
	if inner == nil {
		cancel()
		return nil, invalid("Anthropic model returned no stream")
	}
	state := &anthropicStreamState{}
	normalized := schema.StreamReaderWithConvert(inner, func(msg *schema.AgenticMessage) (*schema.AgenticMessage, error) {
		if msg == nil {
			return nil, invalid("Anthropic stream returned no message")
		}
		if state.finish != "" {
			return nil, invalid("Anthropic stream continued after terminal stop reason")
		}
		state.messages = append(state.messages, msg)
		if finish, ok := anthropicFinish(msg); ok {
			state.finish = finish
		}
		if msg.ResponseMeta != nil && msg.ResponseMeta.TokenUsage != nil {
			msg.ResponseMeta.TokenUsage = nil
		}
		return msg, nil
	}, schema.WithErrWrapper(safeModelError), schema.WithOnEOF(func() (any, error) {
		if requestCtx.Err() != nil {
			return nil, requestCtx.Err()
		}
		if state.finish == "" || !capture.Snapshot().MessageStopped {
			return nil, invalid("Anthropic stream ended without terminal stop evidence")
		}
		combined, err := schema.ConcatAgenticMessages(state.messages)
		if err != nil {
			return nil, err
		}
		seen, valid := validFunctionToolCalls(combined)
		if !valid {
			return nil, invalid("Anthropic function tool call is incomplete")
		}
		finish, original, err := normalizeAnthropicStreamFinish(state.finish, seen)
		if err != nil {
			return nil, err
		}
		terminal := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ResponseMeta: &schema.AgenticResponseMeta{ClaudeExtension: &claude.ResponseMetaExtension{StopReason: state.finish}}}
		applyCollectedUsage(terminal, capture)
		return addNormalizedFinish(terminal, finish, original), nil
	}))
	return closeAwareChatStream(normalized, cancel), nil
}

func normalizeAnthropicStreamFinish(raw string, hasToolCall bool) (string, string, error) {
	switch raw {
	case "end_turn", "stop_sequence":
		return "stop", raw, nil
	case "tool_use":
		if !hasToolCall {
			return "", raw, invalid("Anthropic tool_use response has no function tool call")
		}
		return "tool_calls", raw, nil
	case "max_tokens":
		return "length", raw, nil
	case "refusal":
		return "refusal", raw, nil
	default:
		return "", raw, invalid("unsupported Anthropic stop reason")
	}
}

type anthropicStreamState struct {
	finish   string
	messages []*schema.AgenticMessage
}
