package llm

import (
	"context"
	"math"
	"net/http"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/agenticgemini"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	geminischema "github.com/cloudwego/eino/schema/gemini"
	product "github.com/ww1489/seasprak/internal/errors"
	"google.golang.org/genai"
)

// RegisterGeminiGenerateContent installs only the Gemini Developer API
// GenerateContent path. Vertex, server-side tools and explicit cache resources
// remain unsupported until they have separate product evidence.
func (c *Catalog) RegisterGeminiGenerateContent(client *http.Client, maxResponseBytes int) error {
	if maxResponseBytes <= 0 {
		return invalid("positive Gemini response collection limit is required")
	}
	copied := observedClient(client, "gemini-generate-content", maxResponseBytes)
	return c.RegisterObservedFactory("gemini-generate-content", func(ctx context.Context, r ResolvedModelConfig) (Model, error) {
		if r.Config.Provider != "google" {
			return nil, unsupported("Gemini provider route is unavailable")
		}
		if r.Config.NoCredentials {
			return nil, product.NewError(product.CodeUnauthenticated, "Gemini requires an explicit request credential")
		}
		auth, ok := RequestCredential(ctx)
		if !ok || auth.Secret == "" {
			return nil, product.NewError(product.CodeUnauthenticated, "Gemini request credential is unavailable")
		}
		if r.Options.ActiveCache {
			return nil, unsupported("Gemini explicit cache resources are not enabled")
		}

		var retryAttempts int32 = 1
		httpOptions := genai.HTTPOptions{
			BaseURL: r.Config.Endpoint,
			RetryOptions: &genai.HTTPRetryOptions{
				Attempts: &retryAttempts,
			},
		}
		apiClient, err := genai.NewClient(ctx, &genai.ClientConfig{
			APIKey:      auth.Secret,
			Backend:     genai.BackendGeminiAPI,
			HTTPClient:  &copied,
			HTTPOptions: httpOptions,
		})
		if err != nil {
			return nil, err
		}
		maxTokens := r.Options.MaxOutputTokens
		thinking, err := geminiThinking(r.Config.Model, r.Options)
		if err != nil {
			return nil, err
		}
		inner, err := agenticgemini.New(ctx, &agenticgemini.Config{Client: apiClient, Model: r.Config.Model, MaxTokens: &maxTokens, ThinkingConfig: thinking})
		if err != nil {
			return nil, err
		}
		return &geminiGenerateContentModel{inner: inner}, nil
	})
}

func geminiThinking(modelName string, options EffectiveOptions) (*genai.ThinkingConfig, error) {
	if options.EffectiveThinking == "" {
		return nil, nil
	}
	if options.EffectiveThinking == "off" {
		if options.NativeThinking != "none" {
			return nil, unsupported("Gemini off requires an explicit disabling mapping")
		}
		// agenticgemini v0.2.5 forwards genai v1.71.0's *int32 budget,
		// preserving an explicit zero. The GenerateContent thinking guide
		// permits zero for these exact models, not 2.5 Pro or Gemini 3:
		// https://ai.google.dev/gemini-api/docs/generate-content/thinking
		// ResolveOptions still requires scoped, verified off capability;
		// this protocol guard does not certify any model or endpoint.
		switch modelName {
		case "gemini-2.5-flash", "gemini-2.5-flash-lite":
			budget := int32(0)
			return &genai.ThinkingConfig{ThinkingBudget: &budget}, nil
		default:
			return nil, unsupported("Gemini thinking off is unavailable for this model")
		}
	}
	level := strings.ToLower(options.NativeThinking)
	var native genai.ThinkingLevel
	switch level {
	case "minimal":
		native = genai.ThinkingLevelMinimal
	case "low":
		native = genai.ThinkingLevelLow
	case "medium":
		native = genai.ThinkingLevelMedium
	case "high":
		native = genai.ThinkingLevelHigh
	default:
		return nil, unsupported("unsupported Gemini thinking mapping")
	}
	config := &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: native}
	if options.ThinkingBudgetTokens > 0 {
		if int64(options.ThinkingBudgetTokens) > int64(math.MaxInt32) {
			return nil, invalid("Gemini thinking budget exceeds provider limit")
		}
		budget := int32(options.ThinkingBudgetTokens)
		config.ThinkingBudget = &budget
	}
	return config, nil
}

type geminiGenerateContentModel struct{ inner Model }

func (*geminiGenerateContentModel) UsesObservedTransport() bool { return true }

func geminiFinish(msg *schema.AgenticMessage) (string, bool) {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.GeminiExtension == nil {
		return "", false
	}
	finish := msg.ResponseMeta.GeminiExtension.FinishReason
	return finish, finish != ""
}

func geminiMessageHasUnsupportedToolContent(messages []*schema.AgenticMessage) bool {
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		for _, block := range msg.ContentBlocks {
			if block == nil {
				continue
			}
			if block.FunctionToolCall != nil || block.FunctionToolResult != nil || block.Type == schema.ContentBlockTypeFunctionToolCall || block.Type == schema.ContentBlockTypeFunctionToolResult {
				return true
			}
		}
	}
	return false
}

func rejectGeminiFunctionTools(opts []model.Option) error {
	common := model.GetCommonOptions(nil, opts...)
	if len(common.Tools) > 0 || len(common.DeferredTools) > 0 || common.ToolSearchTool != nil {
		return unsupported("Gemini function tools are unavailable without provider call identities")
	}
	return nil
}

func normalizeGeminiFinish(msg *schema.AgenticMessage) (string, string, error) {
	if msg == nil || msg.Role != schema.AgenticRoleTypeAssistant {
		return "", "", invalid("Gemini response role is not assistant")
	}
	raw, ok := geminiFinish(msg)
	if !ok {
		return "", "", invalid("missing Gemini terminal finish reason")
	}
	switch raw {
	case "STOP":
		return "stop", raw, nil
	case "MAX_TOKENS":
		return "length", raw, nil
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return "refusal", raw, nil
	default:
		return "", raw, invalid("unsupported Gemini finish reason")
	}
}

func (m *geminiGenerateContentModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	if err := rejectGeminiFunctionTools(opts); err != nil {
		return nil, err
	}
	if geminiMessageHasUnsupportedToolContent(in) {
		return nil, unsupported("Gemini function tool content is unavailable")
	}
	capture := &usageCapture{}
	requestCtx := withUsageCapture(ctx, capture)
	msg, err := m.inner.Generate(requestCtx, in, opts...)
	if err != nil {
		return nil, safeModelError(err)
	}
	finish, original, err := normalizeGeminiFinish(msg)
	if err != nil {
		return nil, err
	}
	applyCollectedUsage(msg, capture)
	return addNormalizedFinish(msg, finish, original), nil
}

func (m *geminiGenerateContentModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	if err := rejectGeminiFunctionTools(opts); err != nil {
		return nil, err
	}
	if geminiMessageHasUnsupportedToolContent(in) {
		return nil, unsupported("Gemini function tool content is unavailable")
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
		return nil, invalid("Gemini model returned no stream")
	}
	state := &geminiStreamState{}
	normalized := schema.StreamReaderWithConvert(inner, func(msg *schema.AgenticMessage) (*schema.AgenticMessage, error) {
		if msg == nil {
			return nil, invalid("Gemini stream returned no message")
		}
		if state.finish != "" {
			return nil, invalid("Gemini stream continued after terminal finish reason")
		}
		state.messages = append(state.messages, msg)
		if finish, ok := geminiFinish(msg); ok {
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
		if state.finish == "" {
			return nil, invalid("Gemini stream ended without terminal finish reason")
		}
		combined, err := schema.ConcatAgenticMessages(state.messages)
		if err != nil {
			return nil, err
		}
		if combined == nil || combined.Role != schema.AgenticRoleTypeAssistant {
			return nil, invalid("Gemini stream response role is not assistant")
		}
		finish, original, err := normalizeGeminiStreamFinish(state.finish)
		if err != nil {
			return nil, err
		}
		terminal := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ResponseMeta: &schema.AgenticResponseMeta{GeminiExtension: &geminischema.ResponseMetaExtension{FinishReason: state.finish}}}
		applyCollectedUsage(terminal, capture)
		return addNormalizedFinish(terminal, finish, original), nil
	}))
	return closeAwareChatStream(normalized, cancel), nil
}

func normalizeGeminiStreamFinish(raw string) (string, string, error) {
	switch raw {
	case "STOP":
		return "stop", raw, nil
	case "MAX_TOKENS":
		return "length", raw, nil
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return "refusal", raw, nil
	default:
		return "", raw, invalid("unsupported Gemini finish reason")
	}
}

type geminiStreamState struct {
	finish   string
	messages []*schema.AgenticMessage
}
