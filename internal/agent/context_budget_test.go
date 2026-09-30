package agent

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	product "github.com/ww1489/seasprak/internal/errors"
)

func TestEstimateRequestBoundsGrowWithUTF8Bytes(t *testing.T) {
	ascii, err := EstimateRequest("sys", []*schema.AgenticMessage{schema.UserAgenticMessage("abcd")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Chinese and emoji are 3 and 4 UTF-8 bytes; both bounds grow with bytes.
	cjk, _ := EstimateRequest("sys", []*schema.AgenticMessage{schema.UserAgenticMessage("中文中文")}, nil)
	emoji, _ := EstimateRequest("sys", []*schema.AgenticMessage{schema.UserAgenticMessage("😀😀😀😀")}, nil)
	if !(ascii.Upper < cjk.Upper && cjk.Upper < emoji.Upper) || cjk.Version != ContextEstimatorVersion {
		t.Fatalf("upper not byte-based: %d %d %d", ascii.Upper, cjk.Upper, emoji.Upper)
	}
	if ascii.Lower*maxBytesPerToken < ascii.Bytes || ascii.Lower > ascii.Upper {
		t.Fatalf("bounds inconsistent: %+v", ascii)
	}
	withTool, _ := EstimateRequest("sys", []*schema.AgenticMessage{schema.UserAgenticMessage("abcd")}, []*schema.ToolInfo{{Name: "t", Desc: strings.Repeat("d", 100)}})
	if withTool.Upper < ascii.Upper+100 {
		t.Fatal("tool schema not counted")
	}
}

func TestContextBudgetHardUsesLowerSoftUsesUpper(t *testing.T) {
	b := ContextBudget{Window: 100, OutputReserve: 40, SoftRatio: 0.5}
	// Hard: only a request whose lower bound cannot fit is rejected.
	if !b.Fits(ContextEstimate{Lower: 60, Upper: 480}) || b.Fits(ContextEstimate{Lower: 61}) {
		t.Fatal("hard boundary wrong")
	}
	// Soft: the upper bound triggers compaction early.
	if b.OverSoft(ContextEstimate{Upper: 10}) || !b.OverSoft(ContextEstimate{Upper: 11}) {
		t.Fatal("soft boundary wrong")
	}
	err := b.Check(ContextEstimate{Lower: 61, Version: ContextEstimatorVersion})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeBudgetExhausted {
		t.Fatalf("err=%v", err)
	}
	if (ContextBudget{}).Check(ContextEstimate{Lower: 1 << 30}) != nil {
		t.Fatal("undeclared window must not be enforced")
	}
}
