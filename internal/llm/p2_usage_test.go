package llm_test

import (
	"reflect"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func usageValue(n int64) llm.UsageValue {
	return llm.UsageValue{Value: n, Known: true, Source: "provider"}
}

func TestP2UsageCumulativeReplacesRatherThanAdds(t *testing.T) {
	var got llm.UsageRecord
	for _, n := range []int64{100, 700, 1000} {
		var err error
		got, err = got.MergeCumulative(llm.UsageRecord{InputTotal: usageValue(n)})
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := got.MergeCumulative(llm.UsageRecord{UncachedInput: usageValue(300), CacheRead: usageValue(600), CacheWrite: usageValue(100), OutputTotal: usageValue(50), Reasoning: usageValue(20)})
	if err != nil {
		t.Fatal(err)
	}
	if got.InputTotal.Value != 1000 || got.OutputTotal.Value != 50 || got.Reasoning.Value != 20 {
		t.Fatalf("double-counted cumulative usage: %+v", got)
	}
	next, err := got.MergeCumulative(llm.UsageRecord{})
	if err != nil || !reflect.DeepEqual(got, next) {
		t.Fatal("omitted fields erased known values", err)
	}
}

func TestP2UsageUnknownAndKnownZeroStayDistinct(t *testing.T) {
	got, err := (llm.UsageRecord{}).MergeCumulative(llm.UsageRecord{CacheRead: usageValue(0)})
	if err != nil {
		t.Fatal(err)
	}
	if !got.CacheRead.Known || got.CacheRead.Value != 0 || got.CacheWrite.Known || got.UncachedInput.Known {
		t.Fatalf("lost usage presence: %+v", got)
	}
}

func TestP2UsageInvalidSampleDoesNotMutatePrevious(t *testing.T) {
	original := llm.UsageRecord{InputTotal: usageValue(1000)}
	for _, field := range []llm.UsageValue{usageValue(-1), {Value: 4, Known: true}} {
		next, err := original.MergeCumulative(llm.UsageRecord{InputTotal: field})
		p2Code(t, err, product.CodeInvalidArgument)
		if next != original {
			t.Fatal("invalid sample changed prior evidence")
		}
	}
}
