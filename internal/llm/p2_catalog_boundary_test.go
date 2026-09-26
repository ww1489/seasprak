package llm_test

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/model"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

// Bound configuration is also enforced at the reused Eino option boundary.
func TestP2CatalogCallOptionsCannotExpandResolvedBudget(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			option model.Option
		}{
			{"output_budget", model.WithMaxTokens(1024)},
			{"model_identity", model.WithModel("unregistered-model")},
			{"opaque_provider_option", model.WrapImplSpecificOptFn(func(v *struct{ Unverified bool }) { v.Unverified = true })},
		} {
			t.Run(tc.name+map[bool]string{false: "/generate", true: "/stream"}[stream], func(t *testing.T) {
				cfg := p2Config()
				catalog := llm.NewCatalog(nil)
				calls := testkit.NewFake()
				p2OK(t, catalog.RegisterInjected(cfg, calls))
				bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
				p2OK(t, err)
				if bound == nil {
					t.Fatal("bound model missing")
				}
				if stream {
					reader, e := bound.Stream(context.Background(), nil, tc.option)
					err = e
					if reader != nil {
						reader.Close()
					}
				} else {
					_, err = bound.Generate(context.Background(), nil, tc.option)
				}
				if err == nil {
					t.Fatal("call option bypassed fixed configuration")
				}
				e, ok := product.AsError(err)
				if !ok || (e.Code != product.CodeInvalidArgument && e.Code != product.CodeUnsupportedCapability) {
					t.Fatalf("unexpected error: %v", err)
				}
				if calls.Calls() != 0 {
					t.Fatal("rejected call reached model")
				}
			})
		}
	}
}
