package llm_test

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/model"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2OptionsCallTimePreservesMinimumAnswer(t *testing.T) {
	cfg := p2Config()
	cfg.Parameters.MinAnswerTokens = 16
	cfg.Capabilities.ThinkingSharesOutput = false
	c := llm.NewCatalog(nil)
	fake := testkit.NewFake()
	p2OK(t, c.RegisterInjected(cfg, fake))
	bound, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	_, err = bound.Generate(context.Background(), nil, model.WithMaxTokens(1))
	p2Code(t, err, product.CodeInvalidArgument)
	if fake.Calls() != 0 {
		t.Fatal("invalid answer limit reached model")
	}
}
