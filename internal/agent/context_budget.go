package agent

import (
	"encoding/json"

	"github.com/cloudwego/eino/schema"

	product "github.com/ww1489/seasprak/internal/errors"
)

// ContextEstimatorVersion identifies the estimator. It measures the UTF-8
// bytes of the serialized request and derives two bounds from them:
//   - Upper: every byte-level BPE/byte-fallback tokenizer emits at most one
//     token per byte, so bytes are a proven upper bound.
//   - Lower: known tokenizers need at least one token per maxBytesPerToken
//     bytes, so bytes/maxBytesPerToken is a lower bound.
//
// The hard check uses the lower bound: it rejects only requests that cannot
// fit, never a request that might. The soft trigger uses the upper bound so
// compaction starts early. Neither is an exact provider count.
const ContextEstimatorVersion = "utf8-bytes-bounds-v1"

const maxBytesPerToken = 8

// DefaultSoftRatio is the automatic compaction threshold: 80% of the window
// available for input after the output reserve (12 §engineering defaults).
const DefaultSoftRatio = 0.8

// ContextBudget is the model window resolved for one request. Window 0 means
// the model declared no window; the request is not window-budgeted.
type ContextBudget struct {
	Window        int
	OutputReserve int
	// SoftRatio triggers compaction before the hard limit; 0 disables it.
	SoftRatio float64
}

// ContextEstimate bounds the size of one complete request in tokens.
type ContextEstimate struct {
	Bytes   int
	Upper   int
	Lower   int
	Version string
}

// EstimateRequest serializes the complete request (instruction, every message
// including media payload bytes, and tool schemas).
func EstimateRequest(instruction string, msgs []*schema.AgenticMessage, tools []*schema.ToolInfo) (ContextEstimate, error) {
	raw, err := json.Marshal(struct {
		Instruction string                   `json:"i"`
		Messages    []*schema.AgenticMessage `json:"m"`
		Tools       []*schema.ToolInfo       `json:"t"`
	}{instruction, msgs, tools})
	if err != nil {
		return ContextEstimate{}, product.NewError(product.CodeInvalidArgument, "request cannot be measured")
	}
	n := len(raw)
	return ContextEstimate{Bytes: n, Upper: n, Lower: (n + maxBytesPerToken - 1) / maxBytesPerToken, Version: ContextEstimatorVersion}, nil
}

// Fits reports whether the request can possibly fit with the output reserve.
func (b ContextBudget) Fits(e ContextEstimate) bool {
	return b.Window <= 0 || e.Lower+b.OutputReserve <= b.Window
}

// OverSoft reports whether compaction should run before this request.
func (b ContextBudget) OverSoft(e ContextEstimate) bool {
	return b.Window > 0 && b.SoftRatio > 0 && float64(e.Upper+b.OutputReserve) > b.SoftRatio*float64(b.Window)
}

// Check rejects a request that cannot fit before any physical request.
func (b ContextBudget) Check(e ContextEstimate) error {
	if !b.Fits(e) {
		return product.Errorf(product.CodeBudgetExhausted, "request needs at least %d tokens plus %d output reserve but the window is %d (%s)", e.Lower, b.OutputReserve, b.Window, e.Version)
	}
	return nil
}
