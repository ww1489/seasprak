package llm_test

import (
	"io"
	"strings"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func TestP2UsageSSEPresenceAndFrameBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		known      bool
		value      int64
	}{
		{"multiline", "data: {\"usage\":\ndata: {\"prompt_tokens\":7}}\n\ndata: [DONE]\n\n", true, 7},
		{"missing", "data: {\"usage\":{\"prompt_tokens\":7}}\n\ndata: {\"usage\":{\"completion_tokens\":2}}\n\n", true, 7},
		{"null", "data: {\"usage\":{\"prompt_tokens\":null}}\n\n", false, 0},
		{"unfinished", "data: {\"usage\":{\"prompt_tokens\":7}}\n", false, 0},
		{"wrong-type", "data: {\"usage\":{\"prompt_tokens\":\"7\"}}\n\n", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, chunk := range []int{1, 4096} {
				c := llm.NewUsageCollector("openai-chat", 4096)
				r := llm.WrapUsageBody(&usageBody{data: []byte(tc.body), chunk: chunk, end: io.EOF}, "text/event-stream", c)
				got, err := io.ReadAll(r)
				p2OK(t, err)
				p2OK(t, r.Close())
				u := c.Snapshot().Usage.InputTotal
				if string(got) != tc.body || u.Known != tc.known || u.Value != tc.value {
					t.Fatalf("framing or presence changed: %+v", c.Snapshot())
				}
			}
		})
	}
}

func TestP2UsageClaudeAbsentCacheWriteRemainsUnknown(t *testing.T) {
	c := llm.NewUsageCollector("anthropic-messages", 1024)
	r := llm.WrapUsageBody(io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":300,"cache_read_input_tokens":600,"output_tokens":50}}`)), "application/json", c)
	_, err := io.ReadAll(r)
	p2OK(t, err)
	p2OK(t, r.Close())
	u := c.Snapshot().Usage
	if u.InputTotal.Known || u.CacheWrite.Known || !u.UncachedInput.Known || u.UncachedInput.Value != 300 {
		t.Fatal("absent cache writes were inferred as zero")
	}
}

func TestP2UsagePriceEstimateRequiresVersion(t *testing.T) {
	original := llm.UsageRecord{InputTotal: usageValue(1000)}
	for _, estimate := range []llm.CostEstimate{
		{Known: true, AmountDecimal: "1", Currency: "fixture"},
		{Known: true, AmountDecimal: "1", PriceVersion: "fixture-v1"},
		{Known: true, Currency: "fixture", PriceVersion: "fixture-v1"},
	} {
		next, err := original.MergeCumulative(llm.UsageRecord{Estimate: estimate})
		p2Code(t, err, product.CodeInvalidArgument)
		if next != original {
			t.Fatal("invalid price changed usage")
		}
	}
	estimate := llm.CostEstimate{Known: true, AmountDecimal: "1.25", Currency: "fixture", PriceVersion: "fixture-v1"}
	next, err := original.MergeCumulative(llm.UsageRecord{Estimate: estimate})
	p2OK(t, err)
	again, err := next.MergeCumulative(llm.UsageRecord{})
	p2OK(t, err)
	if next.Estimate != estimate || again != next || original.Estimate.Known {
		t.Fatal("price estimate overwritten or invented")
	}
}
