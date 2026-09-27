package llm

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"sync"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/openai"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// RegisterOpenAIResponses installs the OpenAI Responses adapter with the
// product's transport and response-acceptance rules. The client is copied and
// cannot redirect credentials to another destination.
func (c *Catalog) RegisterOpenAIResponses(client *http.Client, maxResponseBytes int) error {
	if maxResponseBytes <= 0 {
		return invalid("positive Responses response collection limit is required")
	}
	copied := observedClient(client, "openai-responses", maxResponseBytes)
	return c.RegisterObservedFactory("openai-responses", func(ctx context.Context, r ResolvedModelConfig) (Model, error) {
		auth, ok := RequestCredential(ctx)
		if !r.Config.NoCredentials && !ok {
			return nil, invalid("Responses request credential missing")
		}

		maxTokens := r.Options.MaxOutputTokens
		maxRetries := 0
		store := false
		config := &agenticopenai.ResponsesConfig{
			APIKey:          auth.Secret,
			BaseURL:         r.Config.Endpoint,
			HTTPClient:      &copied,
			MaxRetries:      &maxRetries,
			Model:           r.Config.Model,
			MaxTokens:       &maxTokens,
			Store:           &store,
			EnableAutoCache: false,
		}
		if r.Options.EffectiveThinking != "" {
			if r.Options.ThinkingBudgetTokens != 0 {
				return nil, unsupported("Responses reasoning token budgets are not supported")
			}
			native := r.Options.NativeThinking
			switch native {
			case "none", "minimal", "low", "medium", "high", "xhigh", "max":
			default:
				return nil, unsupported("unsupported Responses thinking mapping")
			}
			if (r.Options.EffectiveThinking == "off") != (native == "none") {
				return nil, unsupported("Responses thinking mapping cannot honor off")
			}
			reasoning := responses.ReasoningParam{Effort: shared.ReasoningEffort(native)}
			config.Reasoning = &reasoning
		}
		if r.Options.ActiveCache {
			retention := responses.ResponseNewParamsPromptCacheRetentionInMemory
			if r.Options.CacheIntent == "long" {
				retention = responses.ResponseNewParamsPromptCacheRetention24h
			} else if r.Options.CacheIntent != "short" {
				return nil, unsupported("unsupported Responses cache retention")
			}
			config.PromptCacheRetention = &retention
		}

		inner, err := agenticopenai.NewResponsesModel(ctx, config)
		if err != nil {
			return nil, err
		}
		return &openAIResponsesModel{inner: inner, secret: auth.Secret}, nil
	})
}

// observedClient creates one transport wrapper per factory registration. The
// underlying adapter remains responsible for HTTP framing and protocol parsing.
func observedClient(client *http.Client, protocol string, maxResponseBytes int) http.Client {
	copied := http.Client{}
	if client != nil {
		copied = *client
	}
	copied.Transport = NewObservedTransport(copied.Transport, UsageCollection{Protocol: protocol, MaxBytes: maxResponseBytes})
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return copied
}

type usageCapture struct {
	mu       sync.Mutex
	snapshot UsageSnapshot
}

func (c *usageCapture) ObserveUsage(_ context.Context, _ TransportRequest, snapshot UsageSnapshot) {
	c.mu.Lock()
	c.snapshot = snapshot
	c.mu.Unlock()
}
func (c *usageCapture) Snapshot() UsageSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot
}

type usageObserverFunc func(context.Context, TransportRequest, UsageSnapshot)

func (f usageObserverFunc) ObserveUsage(ctx context.Context, request TransportRequest, snapshot UsageSnapshot) {
	f(ctx, request, snapshot)
}

// withUsageCapture records raw presence for the adapter result while retaining
// the caller's observer. A missing field never becomes a known zero.
func withUsageCapture(ctx context.Context, capture *usageCapture) context.Context {
	previous, _ := ctx.Value(usageObservationKey{}).(UsageObserver)
	return WithUsageObservation(ctx, usageObserverFunc(func(ctx context.Context, request TransportRequest, snapshot UsageSnapshot) {
		capture.ObserveUsage(ctx, request, snapshot)
		if previous != nil {
			previous.ObserveUsage(ctx, request, snapshot)
		}
	}))
}

func preserveKnownUsage(msg *schema.AgenticMessage, capture *usageCapture) {
	if msg == nil || msg.ResponseMeta == nil {
		return
	}
	usage := capture.Snapshot().Usage
	if !usage.InputTotal.Known || !usage.OutputTotal.Known {
		msg.ResponseMeta.TokenUsage = nil
	}
}

func hasFunctionToolCall(msg *schema.AgenticMessage) bool {
	if msg == nil {
		return false
	}
	for _, block := range msg.ContentBlocks {
		if block != nil && block.FunctionToolCall != nil {
			return true
		}
	}
	return false
}

func hasRefusal(msg *schema.AgenticMessage) bool {
	if msg == nil {
		return false
	}
	for _, block := range msg.ContentBlocks {
		if block == nil || block.AssistantGenText == nil || block.AssistantGenText.OpenAIExtension == nil {
			continue
		}
		if block.AssistantGenText.OpenAIExtension.Refusal != nil {
			return true
		}
	}
	return false
}

func addNormalizedFinish(msg *schema.AgenticMessage, finish, original string) *schema.AgenticMessage {
	if msg == nil {
		return nil
	}
	msg.Extra = maps.Clone(msg.Extra)
	if msg.Extra == nil {
		msg.Extra = map[string]any{}
	}
	msg.Extra["seasprak.finish"] = finish
	msg.Extra["seasprak.original_finish"] = original
	return msg
}

func responseTerminalStatus(msg *schema.AgenticMessage) (status, reason string, ok bool) {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.OpenAIExtension == nil {
		return "", "", false
	}
	ext := msg.ResponseMeta.OpenAIExtension
	status = string(ext.Status)
	if ext.IncompleteDetails != nil {
		reason = ext.IncompleteDetails.Reason
	}
	return status, reason, status != ""
}

func normalizeResponsesFinish(ctx context.Context, msg *schema.AgenticMessage) (string, string, error) {
	status, reason, ok := responseTerminalStatus(msg)
	if !ok {
		return "", "", invalid("missing Responses terminal status")
	}
	if hasRefusal(msg) {
		if status == string(openai.ResponseStatusCompleted) || (status == string(openai.ResponseStatusIncomplete) && reason == "content_filter") {
			return "refusal", status, nil
		}
	}
	switch status {
	case string(openai.ResponseStatusCompleted):
		if hasFunctionToolCall(msg) {
			return "tool_calls", status, nil
		}
		return "stop", status, nil
	case string(openai.ResponseStatusIncomplete):
		switch reason {
		case "max_output_tokens":
			return "length", reason, nil
		case "content_filter":
			return "refusal", reason, nil
		default:
			return "", reason, invalid("unsupported Responses incomplete reason")
		}
	case string(openai.ResponseStatusFailed):
		return "", status, safeModelError(errors.New("Responses response failed"))
	case string(openai.ResponseStatusCancelled):
		if ctx.Err() != nil {
			return "", status, ctx.Err()
		}
		return "", status, safeModelError(errors.New("Responses response cancelled"))
	default:
		return "", status, invalid("Responses response did not reach a terminal status")
	}
}

type openAIResponsesModel struct {
	inner  Model
	secret string
}

func (*openAIResponsesModel) UsesObservedTransport() bool { return true }

func (m *openAIResponsesModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	capture := &usageCapture{}
	requestCtx := withUsageCapture(ctx, capture)
	msg, err := m.inner.Generate(requestCtx, in, opts...)
	if err != nil {
		return nil, safeModelError(err)
	}
	finish, original, err := normalizeResponsesFinish(requestCtx, msg)
	if err != nil {
		return nil, err
	}
	preserveKnownUsage(msg, capture)
	return addNormalizedFinish(msg, finish, original), nil
}

func (m *openAIResponsesModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
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
		return nil, invalid("Responses model returned no stream")
	}
	state := &responsesStreamState{}
	normalized := schema.StreamReaderWithConvert(inner, func(msg *schema.AgenticMessage) (*schema.AgenticMessage, error) {
		if msg == nil {
			return nil, invalid("Responses stream returned no message")
		}
		if state.terminal != "" {
			return nil, invalid("Responses stream continued after terminal status")
		}
		if status, reason, ok := responseTerminalStatus(msg); ok {
			if isResponsesTerminal(status) {
				state.terminal, state.reason = status, reason
			} else if status != string(openai.ResponseStatusInProgress) && status != string(openai.ResponseStatusQueued) {
				return nil, invalid("Responses stream returned unsupported status")
			}
		}
		state.toolCalls = state.toolCalls || hasFunctionToolCall(msg)
		state.refusal = state.refusal || hasRefusal(msg)
		if msg.ResponseMeta != nil && msg.ResponseMeta.TokenUsage != nil {
			state.usage = msg.ResponseMeta.TokenUsage
			msg.ResponseMeta.TokenUsage = nil
		}
		return msg, nil
	}, schema.WithErrWrapper(safeModelError), schema.WithOnEOF(func() (any, error) {
		if requestCtx.Err() != nil {
			return nil, requestCtx.Err()
		}
		if state.terminal == "" {
			return nil, invalid("Responses stream ended without terminal status")
		}
		terminal := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ResponseMeta: &schema.AgenticResponseMeta{TokenUsage: state.usage}}
		finish, original, err := normalizeResponsesFinishWithState(requestCtx, terminal, state)
		if err != nil {
			return nil, err
		}
		preserveKnownUsage(terminal, capture)
		return addNormalizedFinish(terminal, finish, original), nil
	}))
	return closeAwareChatStream(normalized, cancel), nil
}

type responsesStreamState struct {
	terminal  string
	reason    string
	toolCalls bool
	refusal   bool
	usage     *schema.TokenUsage
}

func isResponsesTerminal(status string) bool {
	switch status {
	case string(openai.ResponseStatusCompleted), string(openai.ResponseStatusIncomplete), string(openai.ResponseStatusFailed), string(openai.ResponseStatusCancelled):
		return true
	default:
		return false
	}
}

func normalizeResponsesFinishWithState(ctx context.Context, msg *schema.AgenticMessage, state *responsesStreamState) (string, string, error) {
	if state.terminal == string(openai.ResponseStatusCompleted) {
		if state.refusal {
			return "refusal", state.terminal, nil
		}
		if state.toolCalls {
			return "tool_calls", state.terminal, nil
		}
		return "stop", state.terminal, nil
	}
	msg.ResponseMeta.OpenAIExtension = &openai.ResponseMetaExtension{Status: openai.ResponseStatus(state.terminal), IncompleteDetails: &openai.IncompleteDetails{Reason: state.reason}}
	return normalizeResponsesFinish(ctx, msg)
}
