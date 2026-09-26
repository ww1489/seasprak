package llm

import (
	"context"
	"errors"
	"maps"

	chatapi "github.com/meguminnnnnnnnn/go-openai"
	"net/http"
	"sync"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	aclopenai "github.com/cloudwego/eino-ext/libs/acl/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// RegisterOpenAIChat explicitly installs only Chat Completions. maxResponseBytes
// bounds a JSON document or SSE frame and must be supplied by trusted assembly.
// The client is copied; its transport must support concurrent requests.
func (c *Catalog) RegisterOpenAIChat(client *http.Client, maxResponseBytes int) error {
	if maxResponseBytes <= 0 {
		return invalid("positive Chat response collection limit is required")
	}
	copied := http.Client{}
	if client != nil {
		copied = *client
	}
	copied.Transport = NewObservedTransport(copied.Transport, UsageCollection{Protocol: "openai-chat", MaxBytes: maxResponseBytes})
	// Redirects can change the credential destination or introduce hidden requests.
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c.RegisterObservedFactory("openai-chat", func(ctx context.Context, r ResolvedModelConfig) (Model, error) {
		extra := map[string]any{}
		if r.Options.EffectiveThinking != "" {
			switch r.Options.NativeThinking {
			case "none", "minimal", "low", "medium", "high", "xhigh":
			default:
				return nil, unsupported("unsupported Chat thinking mapping")
			}
			if (r.Options.EffectiveThinking == "off") != (r.Options.NativeThinking == "none") {
				return nil, unsupported("Chat thinking mapping cannot honor off")
			}
			if r.Options.ThinkingBudgetTokens != 0 {
				return nil, unsupported("Chat effort cannot enforce a thinking token budget")
			}
			extra["reasoning_effort"] = r.Options.NativeThinking
		}
		// Verified Chat cache capabilities certify the native retention field;
		// no cache key, resource or stateful continuation is synthesized.
		if r.Options.ActiveCache {
			switch r.Options.CacheIntent {
			case "short":
				extra["prompt_cache_retention"] = "in_memory"
			case "long":
				extra["prompt_cache_retention"] = "24h"
			default:
				return nil, unsupported("unsupported Chat cache retention")
			}
		}
		auth, ok := RequestCredential(ctx)
		if !r.Config.NoCredentials && !ok {
			return nil, invalid("Chat request credential missing")
		}
		inner, err := agenticopenai.NewChatModel(ctx, &agenticopenai.ChatConfig{APIKey: auth.Secret, BaseURL: r.Config.Endpoint, Model: r.Config.Model, HTTPClient: &copied, MaxCompletionTokens: &r.Options.MaxOutputTokens, ExtraFields: extra})
		if err != nil {
			return nil, err
		}
		// agenticopenai Chat v0.2.4 uses go-openai v0.1.6, whose Generate/Stream
		// each call HTTPClient.Do once and expose no retry setting. No retry layer
		// is added here; physical occupancy still passes through observed transport.
		capability := r.Config.Capabilities.Capability(CapContextOverflow)
		certified := capability.Status == Verified && validateCapability(capability, r.Config) == nil
		return &openAIChatModel{inner: inner, limit: maxResponseBytes, secret: auth.Secret, overflowCertified: certified}, nil
	})
}

type openAIChatModel struct {
	inner             Model
	limit             int
	secret            string // Request-local snapshot, including the entire stream.
	overflowCertified bool
}

func (*openAIChatModel) UsesObservedTransport() bool { return true }
func (m *openAIChatModel) request(ctx context.Context) (context.Context, *UsageCollector) {
	c := NewUsageCollector("openai-chat", m.limit)
	c.chat = &chatCollection{}
	return context.WithValue(ctx, chatCollectorKey{}, c), c
}

// Catalog has already bounded MaxTokens. Translate it, rather than emitting
// conflicting max_tokens and max_completion_tokens or keeping a larger default.
func chatOutputOptions(opts []model.Option) []model.Option {
	common := model.GetCommonOptions(nil, opts...)
	if common.MaxTokens == nil {
		return opts
	}
	out := append([]model.Option(nil), opts...)
	return append(out, model.WithMaxTokens(0), aclopenai.WithMaxCompletionTokens(*common.MaxTokens))
}

func chatFinish(msg *schema.AgenticMessage) string {
	if msg != nil && msg.ResponseMeta != nil {
		if ext, ok := msg.ResponseMeta.Extension.(*agenticopenai.ChatResponseMetaExtension); ok && ext != nil {
			return ext.FinishReason
		}
	}
	return ""
}
func normalizedChatFinish(finish string, refused bool) (string, error) {
	if refused {
		return "refusal", nil
	}
	switch finish {
	case "stop", "tool_calls", "length":
		return finish, nil
	case "content_filter":
		return "refusal", nil
	default:
		return "", invalid("missing or unsupported Chat finish reason")
	}
}
func (m *openAIChatModel) chatRequestError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	// The SDK's finite code is considered only alongside this request's actual
	// HTTP status. Message text and arbitrary retry flags never classify overflow.
	o, _ := ctx.Value(failureObservationKey{}).(*failureObservation)
	var api *chatapi.APIError
	if m.overflowCertified && o != nil && errors.As(err, &api) && api != nil && api.HTTPStatusCode == 400 && api.Code == "context_length_exceeded" && vetoCode(err) == "" && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		o.mu.Lock()
		if o.info.Kind == "invalid_request" {
			o.info = FailureClassification{Kind: "context_overflow"}
		}
		o.mu.Unlock()
	}
	return observedModelError(ctx, err)
}

func (m *openAIChatModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	ctx, c := m.request(ctx)
	opts = chatOutputOptions(opts)
	msg, err := m.inner.Generate(ctx, in, opts...)
	if err != nil {
		return nil, m.chatRequestError(ctx, err)
	}
	refused, usage, err := c.chatResult()
	if err != nil {
		return nil, err
	}
	finish, err := normalizedChatFinish(chatFinish(msg), refused)
	if err != nil {
		return nil, err
	}
	msg.Extra = maps.Clone(msg.Extra)
	if msg.Extra == nil {
		msg.Extra = map[string]any{}
	}
	msg.Extra["seasprak.finish"] = finish
	msg.Extra["seasprak.original_finish"] = originalChatFinish(chatFinish(msg))
	addChatRefusal(msg, c.chatRefusal(m.secret))
	if msg.ResponseMeta != nil && (!usage.InputTotal.Known || !usage.OutputTotal.Known) {
		msg.ResponseMeta.TokenUsage = nil
	}
	return msg, nil
}
func (m *openAIChatModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	ctx, cancel := context.WithCancel(ctx)
	ctx, c := m.request(ctx)
	opts = chatOutputOptions(opts)
	reader, err := m.inner.Stream(ctx, in, opts...)
	if err != nil {
		cancel()
		c.closeChatBody()
		return nil, m.chatRequestError(ctx, err)
	}
	finish := ""
	lastBlockIndex := -1
	var sdkUsage *schema.TokenUsage
	// Only the terminal synthetic metadata chunk carries the normalized finish.
	// Intermediate tool deltas remain provisional until EOF verification in L2.
	normalized := schema.StreamReaderWithConvert(reader, func(msg *schema.AgenticMessage) (*schema.AgenticMessage, error) {
		for _, block := range msg.ContentBlocks {
			if block != nil && block.StreamingMeta != nil {
				lastBlockIndex = max(lastBlockIndex, block.StreamingMeta.Index)
			}
		}
		if f := chatFinish(msg); f != "" {
			if finish != "" && finish != f {
				return nil, invalid("conflicting Chat finish reasons")
			}
			finish = f
		}
		if msg.ResponseMeta != nil {
			if msg.ResponseMeta.TokenUsage != nil {
				sdkUsage = msg.ResponseMeta.TokenUsage
			}
			msg.ResponseMeta.TokenUsage = nil
		}
		return msg, nil
	}, schema.WithErrWrapper(safeModelError), schema.WithOnEOF(func() (any, error) {
		refused, usage, e := c.chatResult()
		if e != nil {
			return nil, e
		}
		reason, e := normalizedChatFinish(finish, refused)
		if e != nil {
			return nil, e
		}
		msg := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, Extra: map[string]any{"seasprak.finish": reason, "seasprak.original_finish": originalChatFinish(finish)}}
		if block := addChatRefusal(msg, c.chatRefusal(m.secret)); block != nil {
			block.StreamingMeta = &schema.StreamingMeta{Index: lastBlockIndex + 1}
		}
		if usage.InputTotal.Known && usage.OutputTotal.Known {
			msg.ResponseMeta = &schema.AgenticResponseMeta{TokenUsage: sdkUsage}
		}
		return msg, nil
	}))
	return closeAwareChatStream(normalized, func() { cancel(); c.closeChatBody() }), nil
}

// Eino's adapter closes its HTTP body only after its producer exits. Its reader
// Close alone cannot interrupt a producer blocked in HTTP Read. An unbuffered
// demand pipe gives Close a cancellation signal without polling or pre-reading:
// while Recv is in flight the sender is blocked sending the next demand token.
func closeAwareChatStream(inner *schema.StreamReader[*schema.AgenticMessage], cancel context.CancelFunc) *schema.StreamReader[*schema.AgenticMessage] {
	demand, writer := schema.Pipe[struct{}](0)
	demand.SetAutomaticClose()
	var once sync.Once
	closeStream := func() { once.Do(func() { cancel(); inner.Close(); demand.Close() }) }
	go func() {
		defer writer.Close()
		defer closeStream()
		for !writer.Send(struct{}{}, nil) {
		}
	}()
	return schema.StreamReaderWithConvert(demand, func(struct{}) (*schema.AgenticMessage, error) {
		msg, err := inner.Recv()
		if err != nil {
			closeStream()
			return msg, err
		}
		return msg, nil
	})
}
