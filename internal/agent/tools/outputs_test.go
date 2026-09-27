package tools

import (
	"testing"

	"github.com/ww1489/seasprak/internal/config"
)

func TestPreviewHeadPreservesUTF8AndReportsContinuation(t *testing.T) {
	input := "第一行🙂\n第二行\n第三行\n"
	got := PreviewHead(input, 2, len([]byte("第一行🙂\n第二行")))
	if got.Text != "第一行🙂\n第二行" || !got.Truncated || got.NextLine != 3 || got.Reason == "" {
		t.Fatalf("unexpected head preview: %+v", got)
	}
}

func TestPreviewTailKeepsTailLinesAndByteBoundary(t *testing.T) {
	input := "old\n中间🙂\n末尾\n"
	got := PreviewTail(input, 2, len([]byte("中间🙂\n末尾")))
	if got.Text != "中间🙂\n末尾" || !got.Truncated || got.StartLine != 2 || got.EndLine != 3 {
		t.Fatalf("unexpected tail preview: %+v", got)
	}
}

func TestBuiltinOutputLimitsMatchP2Defaults(t *testing.T) {
	limits := DefaultOutputLimits()
	if limits.MaxLines != 2000 || limits.MaxBytes != 50<<10 || limits.MaxMatchBytes != 500 {
		t.Fatalf("unexpected output limits: %+v", limits)
	}
	_ = config.ToolOutputChunkBytes
}
