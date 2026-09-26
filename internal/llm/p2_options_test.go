package llm_test

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2OptionsMinimumAnswerCallLimit(t *testing.T) {
	cfg := p2Config()
	cfg.Parameters.MinAnswerTokens = 32
	c := llm.NewCatalog(nil)
	fake := testkit.NewFake()
	built := 0
	p2OK(t, c.RegisterFactory(cfg.Protocol, func(context.Context, llm.ResolvedModelConfig) (llm.Model, error) { built++; return fake, nil }))
	p2OK(t, c.Register(cfg))
	m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	_, err = m.Generate(context.Background(), nil, model.WithMaxTokens(16))
	p2Code(t, err, product.CodeInvalidArgument)
	if built != 0 || fake.Calls() != 0 {
		t.Fatal("answer limit violation reached model")
	}
}

func TestP2OptionsDeclaredThinkingCannotBecomeVerified(t *testing.T) {
	cfg := p2Config()
	d := cfg.Capabilities.Items[llm.CapText]
	cfg.Capabilities.Thinking = map[string]llm.ThinkingSupport{"high": {Capability: d, NativeValue: "high"}}
	for _, level := range []string{"minimal", "high", "max", "off"} {
		_, err := llm.ResolveOptions(cfg, llm.RequestedOptions{Thinking: level})
		p2Code(t, err, product.CodeUnsupportedCapability)
	}
	got, err := llm.ResolveOptions(cfg, llm.RequestedOptions{})
	p2OK(t, err)
	if got.EffectiveThinking != "" {
		t.Fatal("unspecified thinking became explicit off or on")
	}
	for _, o := range []llm.RequestedOptions{{Thinking: "typo"}, {CacheIntent: "typo"}, {MaxOutputTokens: 513}, {MaxOutputTokens: -1}} {
		_, err = llm.ResolveOptions(cfg, o)
		p2Code(t, err, product.CodeInvalidArgument)
	}
}

func TestP2CatalogModalitiesAndServerContent(t *testing.T) {
	cfg := p2Config()
	c := llm.NewCatalog(nil)
	fake := testkit.NewFake()
	built := 0
	p2OK(t, c.RegisterFactory(cfg.Protocol, func(context.Context, llm.ResolvedModelConfig) (llm.Model, error) { built++; return fake, nil }))
	p2OK(t, c.Register(cfg))
	m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	for _, block := range []*schema.ContentBlock{
		schema.NewContentBlock(&schema.UserInputImage{}), schema.NewContentBlock(&schema.UserInputAudio{}), schema.NewContentBlock(&schema.UserInputVideo{}), schema.NewContentBlock(&schema.UserInputFile{}),
		schema.NewContentBlock(&schema.ServerToolCall{}),
		{Type: schema.ContentBlockTypeUserInputText, UserInputImage: &schema.UserInputImage{}},
		schema.NewContentBlock(&schema.FunctionToolResult{Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeImage, Image: &schema.UserInputImage{}}}}),
		schema.NewContentBlock(&schema.FunctionToolResult{Content: []*schema.FunctionToolResultContentBlock{{Type: "unknown_modality"}}}),
	} {
		_, err = m.Generate(context.Background(), []*schema.AgenticMessage{{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{block}}})
		p2Code(t, err, product.CodeUnsupportedCapability)
	}
	if built != 0 || fake.Calls() != 0 {
		t.Fatal("unsupported input reached model")
	}
	cfg.Version = "v2"
	cfg.Capabilities.Items[llm.CapInputImage] = cfg.Capabilities.Items[llm.CapText]
	p2OK(t, c.Register(cfg))
	m, err = c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	_, err = m.Generate(context.Background(), []*schema.AgenticMessage{{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.UserInputImage{})}}})
	p2OK(t, err)
	if built != 1 || fake.Calls() != 1 {
		t.Fatal("declared input modality did not reach model")
	}
}
