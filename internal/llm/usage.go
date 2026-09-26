package llm

// UsageValue distinguishes an absent measurement from an explicitly reported zero.
type UsageValue struct {
	Value  int64  `json:"value"`
	Known  bool   `json:"known"`
	Source string `json:"source,omitempty"`
}

// UsageRecord contains normalized totals and subsets. Cache/reasoning subsets
// must never be blindly added to input/output totals.
type UsageRecord struct {
	InputTotal    UsageValue `json:"inputTotal"`
	UncachedInput UsageValue `json:"uncachedInput"`
	CacheRead     UsageValue `json:"cacheRead"`
	CacheWrite    UsageValue `json:"cacheWrite"`
	OutputTotal   UsageValue `json:"outputTotal"`
	Reasoning     UsageValue `json:"reasoning"`
	// Retention-specific cache writes are subsets, not additional input tokens.
	CacheWrite5Minutes UsageValue   `json:"cacheWrite5Minutes"`
	CacheWrite1Hour    UsageValue   `json:"cacheWrite1Hour"`
	Estimate           CostEstimate `json:"estimate"`
}

// CostEstimate is supplied by a separately versioned pricing policy. No default
// rates or invented zero-cost estimates are produced by usage collection.
type CostEstimate struct {
	Known         bool   `json:"known"`
	AmountDecimal string `json:"amountDecimal,omitempty"`
	Currency      string `json:"currency,omitempty"`
	PriceVersion  string `json:"priceVersion,omitempty"`
}

// MergeCumulative overlays reported fields; omission preserves previous evidence.
func (u UsageRecord) MergeCumulative(next UsageRecord) (UsageRecord, error) {
	result := u
	for _, pair := range []struct {
		dst *UsageValue
		src UsageValue
	}{
		{&result.InputTotal, next.InputTotal}, {&result.UncachedInput, next.UncachedInput},
		{&result.CacheRead, next.CacheRead}, {&result.CacheWrite, next.CacheWrite},
		{&result.OutputTotal, next.OutputTotal}, {&result.Reasoning, next.Reasoning},
		{&result.CacheWrite5Minutes, next.CacheWrite5Minutes}, {&result.CacheWrite1Hour, next.CacheWrite1Hour},
	} {
		if !pair.src.Known {
			continue
		}
		if pair.src.Value < 0 || pair.src.Source == "" {
			return u, invalid("usage measurement requires a nonnegative value and source")
		}
		*pair.dst = pair.src
	}
	if next.Estimate.Known {
		if next.Estimate.AmountDecimal == "" || next.Estimate.Currency == "" || next.Estimate.PriceVersion == "" {
			return u, invalid("cost estimate requires amount, currency and price version")
		}
		result.Estimate = next.Estimate
	}
	return result, nil
}
