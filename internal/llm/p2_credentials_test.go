package llm_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

type p2ErrorStream struct{ secret string }

func (m p2ErrorStream) Generate(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
	return nil, errors.New(m.secret)
}
func (m p2ErrorStream) Stream(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	r, w := schema.Pipe[*schema.AgenticMessage](1)
	w.Send(nil, errors.New(m.secret))
	w.Close()
	return r, nil
}

func TestP2CredentialFactoryGenerateAndStreamErrors(t *testing.T) {
	marker := "synthetic-error-body-marker"
	for _, path := range []string{"factory", "generate", "stream"} {
		t.Run(path, func(t *testing.T) {
			c := llm.NewCatalog(nil)
			cfg := p2Config()
			built := 0
			p2OK(t, c.RegisterFactory(cfg.Protocol, func(context.Context, llm.ResolvedModelConfig) (llm.Model, error) {
				built++
				if path == "factory" {
					return nil, errors.New(marker)
				}
				return p2ErrorStream{marker}, nil
			}))
			p2OK(t, c.Register(cfg))
			m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, err)
			if path == "stream" {
				r, e := m.Stream(context.Background(), nil)
				p2OK(t, e)
				_, err = r.Recv()
				r.Close()
			} else {
				_, err = m.Generate(context.Background(), nil)
			}
			p2Code(t, err, product.CodeResourceUnavailable)
			if strings.Contains(err.Error(), marker) || built != 1 {
				t.Fatal("unsafe error or incorrect factory count")
			}
		})
	}
}

func TestP2CredentialStreamSnapshotAndRevocation(t *testing.T) {
	cfg := p2Config()
	cfg.NoCredentials = false
	cfg.CredentialRef = "test-ref"
	auth := llm.ResolvedCredential{Secret: "synthetic-original", Provider: cfg.Provider, Endpoint: cfg.Endpoint, AccountScope: cfg.AccountScope}
	var requestCtx context.Context
	resolves, built := 0, 0
	var revoked bool
	c := llm.NewCatalog(p2Resolver(func(context.Context, string) (llm.ResolvedCredential, error) {
		resolves++
		if revoked {
			return llm.ResolvedCredential{}, errors.New("revoked")
		}
		return auth, nil
	}))
	p2OK(t, c.RegisterFactory(cfg.Protocol, func(ctx context.Context, _ llm.ResolvedModelConfig) (llm.Model, error) {
		built++
		requestCtx = ctx
		value, ok := llm.RequestCredential(ctx)
		if !ok {
			t.Fatal("missing authentication")
		}
		// Adapter snapshots authentication into a per-request client, never shared.
		return testkit.NewFake(testkit.Step{Text: value.Secret}), nil
	}))
	p2OK(t, c.Register(cfg))
	m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	r, err := m.Stream(context.Background(), nil)
	p2OK(t, err)
	defer r.Close()
	if value, ok := llm.RequestCredential(requestCtx); ok || value.Secret != "" {
		t.Fatal("short-lived credential lease retained secret")
	}
	auth.Secret = "synthetic-next"
	revoked = true
	msg, err := r.Recv()
	p2OK(t, err)
	if textOf(msg) != "synthetic-original" {
		t.Fatal("active stream changed authentication")
	}
	_, err = r.Recv()
	if err != io.EOF {
		t.Fatal(err)
	}
	_, err = m.Generate(context.Background(), nil)
	p2Code(t, err, product.CodeUnauthenticated)
	if resolves != 2 || built != 1 {
		t.Fatalf("resolves/factory=%d/%d", resolves, built)
	}
}

func TestP2CatalogConcurrentRequestsAndSnapshots(t *testing.T) {
	cfg := p2Config()
	c := llm.NewCatalog(nil)
	fake := testkit.NewFake()
	var built atomic.Int32
	p2OK(t, c.RegisterFactory(cfg.Protocol, func(_ context.Context, r llm.ResolvedModelConfig) (llm.Model, error) {
		built.Add(1)
		r.Config.Capabilities.Items[llm.CapText].Evidence[0] = "factory mutation"
		return fake, nil
	}))
	p2OK(t, c.Register(cfg))
	m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			v, e := c.Lookup(cfg.Key())
			if e != nil {
				t.Error(e)
				return
			}
			v.Capabilities.Items[llm.CapText].Evidence[0] = "snapshot mutation"
			_, e = m.Generate(context.Background(), nil)
			if e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if built.Load() != 16 || fake.Calls() != 16 {
		t.Fatal("concurrent invocation count mismatch")
	}
	got, err := c.Lookup(cfg.Key())
	p2OK(t, err)
	if got.Capabilities.Items[llm.CapText].Evidence[0] != cfg.Capabilities.Items[llm.CapText].Evidence[0] {
		t.Fatal("shared snapshot mutated")
	}
}

func TestP2CatalogCommonOptionsAndInvalidZeroRequests(t *testing.T) {
	cfg := p2Config()
	c := llm.NewCatalog(nil)
	fake := testkit.NewFake()
	built := 0
	p2OK(t, c.RegisterFactory(cfg.Protocol, func(context.Context, llm.ResolvedModelConfig) (llm.Model, error) { built++; return fake, nil }))
	p2OK(t, c.Register(cfg))
	m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	for _, tc := range []struct {
		options []model.Option
		code    string
	}{
		{[]model.Option{model.WithModel("other")}, product.CodeInvalidArgument},
		{[]model.Option{model.WithMaxTokens(129)}, product.CodeInvalidArgument},
		{[]model.Option{model.WithMaxTokens(-1)}, product.CodeInvalidArgument},
		{[]model.Option{model.WithToolSearchTool(testkit.ToolInfo("remote", "remote"))}, product.CodeUnsupportedCapability},
		{[]model.Option{model.WrapImplSpecificOptFn(func(v *struct{ RemoteTools bool }) { v.RemoteTools = true })}, product.CodeUnsupportedCapability},
	} {
		_, err = m.Generate(context.Background(), nil, tc.options...)
		p2Code(t, err, tc.code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = m.Generate(ctx, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if built != 0 || fake.Calls() != 0 {
		t.Fatal("invalid requests reached factory")
	}
	_, err = m.Generate(context.Background(), nil, model.WithTools([]*schema.ToolInfo{testkit.ToolInfo("local", "local")}), model.WithMaxTokens(64))
	p2OK(t, err)
	if built != 1 || fake.Calls() != 1 {
		t.Fatal("valid common options did not reach model")
	}
}

func TestP2CatalogIdentityValidationAndEvidence(t *testing.T) {
	c := llm.NewCatalog(nil)
	cfg := p2Config()
	p2OK(t, c.Register(cfg))
	for _, change := range []func(*llm.ModelConfig){func(c *llm.ModelConfig) { c.Provider = "other" }, func(c *llm.ModelConfig) { c.Protocol = "other" }, func(c *llm.ModelConfig) { c.Model = "other" }, func(c *llm.ModelConfig) { c.Version = "v2" }} {
		other := p2Config()
		change(&other)
		p2OK(t, c.Register(other))
		got, err := c.Lookup(other.Key())
		p2OK(t, err)
		if got.Key() != other.Key() {
			t.Fatal("identity alias")
		}
	}
	for _, change := range []func(*llm.ModelConfig){
		func(c *llm.ModelConfig) { c.Model = "" }, func(c *llm.ModelConfig) { c.Endpoint = "https://user:synthetic@example.invalid" }, func(c *llm.ModelConfig) { c.Endpoint = "https://example.invalid?key=synthetic" },
		func(c *llm.ModelConfig) {
			v := c.Capabilities.Items[llm.CapText]
			v.Evidence = nil
			c.Capabilities.Items[llm.CapText] = v
		},
		func(c *llm.ModelConfig) {
			v := c.Capabilities.Items[llm.CapText]
			v.Status = llm.Verified
			c.Capabilities.Items[llm.CapText] = v
		},
	} {
		other := p2Config()
		change(&other)
		p2Code(t, llm.NewCatalog(nil).Register(other), product.CodeInvalidArgument)
	}
}
