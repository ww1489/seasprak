package llm

import (
	"context"
	"net/http"

	"github.com/cloudwego/eino-ext/components/model/agenticdeepseek"
	"github.com/cloudwego/eino-ext/libs/acl/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// RegisterDeepSeekChat installs DeepSeek's native chat adapter. Its response
// metadata is kept separate from the OpenAI Responses metadata path.
func (c *Catalog) RegisterDeepSeekChat(client *http.Client, maxResponseBytes int) error {
	if maxResponseBytes <= 0 {
		return invalid("positive DeepSeek response collection limit is required")
	}
	copied := observedClient(client, "deepseek-chat", maxResponseBytes)
	return c.RegisterObservedFactory("deepseek-chat", func(ctx context.Context, r ResolvedModelConfig) (Model, error) {
		auth, ok := RequestCredential(ctx)
		if !r.Config.NoCredentials && !ok {
			return nil, invalid("DeepSeek request credential missing")
		}
		if r.Options.ThinkingBudgetTokens != 0 {
			return nil, unsupported("DeepSeek independent thinking budget is not supported")
		}
		thinking := r.Options.NativeThinking
		if r.Options.EffectiveThinking != "" {
			want := "enabled"
			if r.Options.EffectiveThinking == "off" {
				want = "disabled"
			}
			if thinking != want {
				return nil, unsupported("DeepSeek thinking mapping must match its native toggle")
			}
		}
		maxTokens := r.Options.MaxOutputTokens
		inner, err := agenticdeepseek.New(ctx, &agenticdeepseek.Config{
			APIKey:     auth.Secret,
			BaseURL:    r.Config.Endpoint,
			HTTPClient: &copied,
			Model:      r.Config.Model,
			MaxTokens:  &maxTokens,
		})
		if err != nil {
			return nil, err
		}
		return &deepSeekModel{inner: inner, secret: auth.Secret, thinking: thinking}, nil
	})
}

type deepSeekModel struct {
	inner    Model
	secret   string
	thinking string
}

// thinkingOptions uses the fixed ACL client's extension point; DeepSeek's
// thinking.type toggle is distinct from OpenAI reasoning_effort.
func (m *deepSeekModel) thinkingOptions(opts []model.Option) []model.Option {
	if m.thinking == "" {
		return opts
	}
	out := append([]model.Option(nil), opts...)
	return append(out, openai.WithExtraFields(map[string]any{"thinking": map[string]any{"type": m.thinking}}))
}

func (*deepSeekModel) UsesObservedTransport() bool { return true }

func deepSeekFinish(msg *schema.AgenticMessage) (string, bool) {
	if msg == nil || msg.ResponseMeta == nil {
		return "", false
	}
	ext, ok := msg.ResponseMeta.Extension.(*agenticdeepseek.ResponseMetaExtension)
	if !ok || ext == nil || ext.FinishReason == "" {
		return "", false
	}
	return ext.FinishReason, true
}

func normalizeDeepSeekFinish(msg *schema.AgenticMessage) (string, string, error) {
	raw, ok := deepSeekFinish(msg)
	if !ok {
		return "", "", invalid("missing DeepSeek terminal finish reason")
	}
	if hasRefusal(msg) {
		return "refusal", raw, nil
	}
	switch raw {
	case "stop":
		return "stop", raw, nil
	case "tool_calls":
		return "tool_calls", raw, nil
	case "length":
		return "length", raw, nil
	case "content_filter":
		return "refusal", raw, nil
	default:
		return "", raw, invalid("unsupported DeepSeek finish reason")
	}
}

func (m *deepSeekModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	capture := &usageCapture{}
	requestCtx := withUsageCapture(ctx, capture)
	msg, err := m.inner.Generate(requestCtx, in, m.thinkingOptions(opts)...)
	if err != nil {
		return nil, safeModelError(err)
	}
	finish, original, err := normalizeDeepSeekFinish(msg)
	if err != nil {
		return nil, err
	}
	applyCollectedUsage(msg, capture)
	return addNormalizedFinish(msg, finish, original), nil
}

func (m *deepSeekModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	requestCtx, cancel := context.WithCancel(ctx)
	capture := &usageCapture{}
	requestCtx = withUsageCapture(requestCtx, capture)
	inner, err := m.inner.Stream(requestCtx, in, m.thinkingOptions(opts)...)
	if err != nil {
		cancel()
		return nil, safeModelError(err)
	}
	if inner == nil {
		cancel()
		return nil, invalid("DeepSeek model returned no stream")
	}
	state := &deepSeekStreamState{}
	normalized := schema.StreamReaderWithConvert(inner, func(msg *schema.AgenticMessage) (*schema.AgenticMessage, error) {
		if msg == nil {
			return nil, invalid("DeepSeek stream returned no message")
		}
		if state.finish != "" {
			return nil, invalid("DeepSeek stream continued after terminal finish reason")
		}
		if finish, ok := deepSeekFinish(msg); ok {
			state.finish = finish
		}
		state.refusal = state.refusal || hasRefusal(msg)
		if msg.ResponseMeta != nil && msg.ResponseMeta.TokenUsage != nil {
			msg.ResponseMeta.TokenUsage = nil
		}
		return msg, nil
	}, schema.WithErrWrapper(safeModelError), schema.WithOnEOF(func() (any, error) {
		if requestCtx.Err() != nil {
			return nil, requestCtx.Err()
		}
		if state.finish == "" {
			return nil, invalid("DeepSeek stream ended without terminal finish reason")
		}
		terminal := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ResponseMeta: &schema.AgenticResponseMeta{Extension: &agenticdeepseek.ResponseMetaExtension{FinishReason: state.finish}}}
		if state.refusal {
			terminal.ContentBlocks = []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{})}
		}
		finish, original, err := normalizeDeepSeekFinish(terminal)
		if err != nil {
			return nil, err
		}
		applyCollectedUsage(terminal, capture)
		return addNormalizedFinish(terminal, finish, original), nil
	}))
	return closeAwareChatStream(normalized, cancel), nil
}

type deepSeekStreamState struct {
	finish  string
	refusal bool
}
