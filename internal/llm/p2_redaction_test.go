package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

type p2BudgetErrorModel struct{ err error }

func (m p2BudgetErrorModel) Generate(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
	return nil, m.err
}
func (m p2BudgetErrorModel) Stream(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{}), m.err
}

func TestP2RedactionPreservesSafeBudgetCode(t *testing.T) {
	const secret = "synthetic-private-error-marker"
	for _, code := range []string{product.CodeBudgetExhausted, product.CodeStorageUnavailable, product.CodePermissionDenied, product.CodeStateConflict} {
		t.Run(code, func(t *testing.T) {
			c := llm.NewCatalog(nil)
			cfg := p2Config()
			cause := &product.Error{Code: code, Message: secret, Retryable: true, Refs: map[string]string{"token": secret}, Details: secret}
			p2OK(t, c.RegisterInjected(cfg, p2BudgetErrorModel{fmt.Errorf("%s: %w", secret, cause)}))
			bound, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, err)
			for _, stream := range []bool{false, true} {
				if stream {
					_, err = bound.Stream(t.Context(), nil)
				} else {
					_, err = bound.Generate(t.Context(), nil)
				}
				p2Code(t, err, code)
				raw, e := json.Marshal(err)
				p2OK(t, e)
				if strings.Contains(string(raw), secret) || strings.Contains(err.Error(), secret) {
					t.Fatal("raw error secret leaked")
				}
				safe, _ := product.AsError(err)
				if safe.Retryable || len(safe.Refs) != 0 || safe.Details != nil {
					t.Fatal("unsafe error metadata retained")
				}
			}
		})
	}
}
