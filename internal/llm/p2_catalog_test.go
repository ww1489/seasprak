package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

func p2Config() llm.ModelConfig {
	d := llm.Capability{Status: llm.Declared, Evidence: []string{"offline test double declaration"}}
	return llm.ModelConfig{Provider: "fake", Protocol: "fake-protocol", Model: "one", Version: "v1", Endpoint: "https://example.invalid/v1", NoCredentials: true, AccountScope: "local", Capabilities: llm.ModelCapabilities{Items: map[llm.CapabilityName]llm.Capability{llm.CapText: d, llm.CapTextStream: d, llm.CapTools: d, llm.CapContextWindow: d, llm.CapOutputLimit: d, llm.CapPhysicalRequestMetering: d}, ContextWindowTokens: 4096, MaxOutputTokens: 512}, Parameters: llm.ModelParameters{MaxOutputTokens: 128, PolicyVersion: "p1"}}
}
func p2Code(t *testing.T, err error, code string) {
	t.Helper()
	e, ok := product.AsError(err)
	if !ok || e.Code != code {
		t.Fatalf("expected code %s, got %v", code, err)
	}
}
func p2OK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestP2CatalogFactoryAndIsolation(t *testing.T) {
	c := llm.NewCatalog(nil)
	config := p2Config()
	fake := testkit.NewFake()
	built := 0
	p2OK(t, c.RegisterFactory(config.Protocol, func(ctx context.Context, r llm.ResolvedModelConfig) (llm.Model, error) {
		built++
		if r.Config.Model != "one" || r.Options.MaxOutputTokens != 128 {
			t.Fatal("factory lost resolved configuration")
		}
		r.Config.Capabilities.Items[llm.CapText] = llm.Capability{Status: llm.Unsupported}
		return fake, nil
	}))
	p2OK(t, c.Register(config))
	config.Capabilities.Items[llm.CapText] = llm.Capability{Status: llm.Unsupported}
	got, err := c.Lookup(config.Key())
	p2OK(t, err)
	if got.Capabilities.Capability(llm.CapText).Status != llm.Declared {
		t.Fatal("catalog retained caller-owned configuration")
	}
	got.Capabilities.Items[llm.CapText].Evidence[0] = "changed"
	got.Capabilities.Items[llm.CapText] = llm.Capability{Status: llm.Unsupported}
	m, err := c.Bind(config.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	if m == nil {
		t.Fatal("catalog did not bind factory")
	}
	_, err = m.Generate(context.Background(), nil)
	p2OK(t, err)
	sr, err := m.Stream(context.Background(), nil)
	p2OK(t, err)
	sr.Close()
	if built != 2 || fake.Calls() != 2 {
		t.Fatalf("factory=%d calls=%d", built, fake.Calls())
	}
	got, err = c.Lookup(config.Key())
	p2OK(t, err)
	if got.Capabilities.Items[llm.CapText].Evidence[0] == "changed" || got.Capabilities.Capability(llm.CapText).Status != llm.Declared {
		t.Fatal("snapshot or factory mutated catalog")
	}
	p2Code(t, c.Register(p2Config()), product.CodeInvalidArgument)
	p2Code(t, c.RegisterFactory(config.Protocol, func(context.Context, llm.ResolvedModelConfig) (llm.Model, error) { return fake, nil }), product.CodeInvalidArgument)
	_, err = c.Bind(llm.ModelKey{}, llm.RequestedOptions{})
	p2Code(t, err, product.CodeInvalidArgument)
}

func TestP2CatalogRejectBeforeFactory(t *testing.T) {
	c := llm.NewCatalog(nil)
	cfg := p2Config()
	fake := testkit.NewFake()
	built := 0
	p2OK(t, c.RegisterFactory(cfg.Protocol, func(context.Context, llm.ResolvedModelConfig) (llm.Model, error) { built++; return fake, nil }))
	p2OK(t, c.Register(cfg))
	for _, o := range []llm.RequestedOptions{{ServerSideEffects: true}, {AutoFailover: true}, {StatefulContinuation: true}, {ExplicitCacheResource: true}, {Thinking: "off"}, {RequiredCapabilities: []llm.CapabilityName{llm.CapInputImage}}} {
		_, err := c.Bind(cfg.Key(), o)
		p2Code(t, err, product.CodeUnsupportedCapability)
	}
	m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	if m == nil {
		t.Fatal("missing bound model")
	}
	_, err = m.Generate(context.Background(), []*schema.AgenticMessage{{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.UserInputImage{URL: "https://example.invalid/image"})}}})
	p2Code(t, err, product.CodeUnsupportedCapability)
	if built != 0 || fake.Calls() != 0 {
		t.Fatalf("rejected request constructed/called model: %d/%d", built, fake.Calls())
	}
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic-messages", "gemini-generate-content", "deepseek-chat"} {
		other := p2Config()
		other.Protocol = protocol
		p2OK(t, c.Register(other))
		_, err = c.Bind(other.Key(), llm.RequestedOptions{})
		p2Code(t, err, product.CodeUnsupportedCapability)
	}
}

func TestP2CatalogInjectedDeclaration(t *testing.T) {
	c := llm.NewCatalog(nil)
	cfg := p2Config()
	fake := testkit.NewFake()
	delete(cfg.Capabilities.Items, llm.CapPhysicalRequestMetering)
	p2Code(t, c.RegisterInjected(cfg, fake), product.CodeUnsupportedCapability)
	cfg = p2Config()
	v := cfg.Capabilities.Items[llm.CapText]
	v.Status = llm.Verified
	cfg.Capabilities.Items[llm.CapText] = v
	p2Code(t, c.RegisterInjected(cfg, fake), product.CodeInvalidArgument)
	cfg = p2Config()
	p2OK(t, c.RegisterInjected(cfg, fake))
	m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	if m == nil {
		t.Fatal("injected model inaccessible")
	}
	_, err = m.Generate(context.Background(), nil)
	p2OK(t, err)
	if fake.Calls() != 1 {
		t.Fatal("injected model not called")
	}
}

func TestP2OptionsThinkingAndCache(t *testing.T) {
	cfg := p2Config()
	v := llm.Capability{Status: llm.Verified, AdapterVersion: "test-v1", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture-v1", ConfigVersion: cfg.Version, Evidence: []string{"offline mapping fixture"}}
	cfg.Capabilities.Thinking = map[string]llm.ThinkingSupport{"low": {Capability: v, NativeValue: "small", BudgetTokens: 20}, "high": {Capability: v, NativeValue: "large", BudgetTokens: 60}}
	cfg.Capabilities.ThinkingSharesOutput = true
	cfg.Parameters.DefaultThinking = "low"
	for _, tc := range []struct{ request, want string }{{"", "low"}, {"minimal", "low"}, {"medium", "high"}, {"xhigh", "high"}, {"max", "high"}} {
		got, err := llm.ResolveOptions(cfg, llm.RequestedOptions{Thinking: tc.request, CacheIntent: "long"})
		p2OK(t, err)
		if got.RequestedThinking != tc.request || got.EffectiveThinking != tc.want || got.CacheIntent != "short" || got.CacheReason == "" || got.Version != "p1" {
			t.Fatalf("wrong effective mapping: %+v", got)
		}
	}
	_, err := llm.ResolveOptions(cfg, llm.RequestedOptions{Thinking: "off"})
	p2Code(t, err, product.CodeUnsupportedCapability)
	_, err = llm.ResolveOptions(cfg, llm.RequestedOptions{Thinking: "medium", ExactThinking: true})
	p2Code(t, err, product.CodeUnsupportedCapability)
	_, err = llm.ResolveOptions(cfg, llm.RequestedOptions{Thinking: "high", MaxOutputTokens: 50})
	p2Code(t, err, product.CodeInvalidArgument)
	got, err := llm.ResolveOptions(cfg, llm.RequestedOptions{CacheIntent: "none"})
	p2OK(t, err)
	if got.ActiveCache || !got.ImplicitCacheMayApply {
		t.Fatal("none promised to disable implicit caching")
	}
	cfg.Capabilities.ContextWindowTokens = 0
	_, err = llm.ResolveOptions(cfg, llm.RequestedOptions{})
	p2Code(t, err, product.CodeUnsupportedCapability)
	cfg.Parameters.ConservativeContextWindow = 2048
	got, err = llm.ResolveOptions(cfg, llm.RequestedOptions{})
	p2OK(t, err)
	if !got.ContextWindowConservative || got.ContextWindowTokens != 2048 {
		t.Fatal("missing conservative window")
	}
}

type p2Resolver func(context.Context, string) (llm.ResolvedCredential, error)

func (r p2Resolver) Resolve(ctx context.Context, ref string) (llm.ResolvedCredential, error) {
	return r(ctx, ref)
}

func TestP2CredentialRotationAndRedaction(t *testing.T) {
	// Synthetic marker only; no environment or credential file is read.
	secret := "synthetic-sensitive-marker"
	cfg := p2Config()
	cfg.NoCredentials = false
	cfg.CredentialRef = "vault/model"
	cfg.AccountScope = "account-a"
	current := llm.ResolvedCredential{Secret: secret, AccountScope: cfg.AccountScope, Provider: cfg.Provider, Endpoint: cfg.Endpoint}
	resolves, built := 0, 0
	var resolveErr error
	c := llm.NewCatalog(p2Resolver(func(context.Context, string) (llm.ResolvedCredential, error) { resolves++; return current, resolveErr }))
	fake := testkit.NewFake()
	p2OK(t, c.RegisterFactory(cfg.Protocol, func(ctx context.Context, r llm.ResolvedModelConfig) (llm.Model, error) {
		built++
		auth, ok := llm.RequestCredential(ctx)
		if !ok || auth.Secret != current.Secret || r.AccountScope != cfg.AccountScope {
			t.Fatal("request credential unavailable")
		}
		return fake, nil
	}))
	p2OK(t, c.Register(cfg))
	m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	if m == nil {
		t.Fatal("missing bound model")
	}
	if resolves != 0 {
		t.Fatal("resolved credentials before request")
	}
	_, err = m.Generate(context.Background(), nil)
	p2OK(t, err)
	current.Secret = "synthetic-rotated-marker"
	_, err = m.Generate(context.Background(), nil)
	p2OK(t, err)
	current.AccountScope = "account-b"
	_, err = m.Generate(context.Background(), nil)
	p2Code(t, err, product.CodeUnauthenticated)
	current.AccountScope = cfg.AccountScope
	current.Endpoint = "https://other.invalid"
	_, err = m.Generate(context.Background(), nil)
	p2Code(t, err, product.CodeUnauthenticated)
	resolveErr = errors.New(secret)
	_, err = m.Generate(context.Background(), nil)
	p2Code(t, err, product.CodeUnauthenticated)
	for _, value := range []any{cfg, current, err} {
		b, e := json.Marshal(value)
		p2OK(t, e)
		if strings.Contains(string(b), secret) || strings.Contains(string(b), current.Secret) {
			t.Fatal("serialized secret")
		}
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", current, current), current.Secret) || strings.Contains(err.Error(), secret) {
		t.Fatal("formatted secret")
	}
	if built != 2 || fake.Calls() != 2 || resolves != 5 {
		t.Fatalf("factory/calls/resolves=%d/%d/%d", built, fake.Calls(), resolves)
	}
}
