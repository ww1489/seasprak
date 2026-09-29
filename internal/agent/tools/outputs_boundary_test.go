package tools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPreviewHeadTailBoundsAndPreservesEnds(t *testing.T) {
	text := "first🙂\n" + strings.Repeat("middle\n", 20) + "last🙂"
	got := PreviewHeadTail(text, 5, 64)
	if !got.Truncated || !strings.HasPrefix(got.Text, "first🙂") || !strings.HasSuffix(got.Text, "last🙂") || len(got.Text) > 64 || len(strings.Split(got.Text, "\n")) > 5 || !utf8.ValidString(got.Text) {
		t.Fatalf("invalid head/tail preview: %+v", got)
	}
	short := "unchanged\r\n"
	if got := PreviewHeadTail(short, 5, 64); got.Text != short || got.Truncated {
		t.Fatalf("short output changed: %+v", got)
	}
	for _, size := range []int{0, 1, 2, 4, 10, 16} {
		got := PreviewHeadTail(strings.Repeat("🙂", 30), 1, size)
		if len(got.Text) > size || !utf8.ValidString(got.Text) || !got.Truncated {
			t.Fatalf("tiny limit %d: %+v", size, got)
		}
	}
}

func TestPreviewHeadTailAllSmallBudgets(t *testing.T) {
	for _, text := range []string{"", "a\n", "\n\n\n\n", "你好🙂\r\n第二行\n最后🙂", strings.Repeat("🙂", 30), strings.Repeat("line\n", 30)} {
		for lines := 1; lines <= 7; lines++ {
			for size := 1; size <= 40; size++ {
				got := PreviewHeadTail(text, lines, size)
				if len(got.Text) > size || len(splitPreviewLines(got.Text)) > lines || !utf8.ValidString(got.Text) {
					t.Fatalf("limits lines=%d bytes=%d produced %+v", lines, size, got)
				}
				if (len(text) > size || len(splitPreviewLines(text)) > lines) && !got.Truncated {
					t.Fatalf("missing truncation for lines=%d bytes=%d", lines, size)
				}
			}
		}
	}
}

func TestPreviewHeadLongSingleLineReportsTruncation(t *testing.T) {
	for _, text := range []string{"abcdef", "你好世界", "🙂🙂🙂"} {
		got := PreviewHead(text, 2000, 5)
		if !got.Truncated || got.Reason != "output_limit" || len(got.Text) > 5 || !utf8.ValidString(got.Text) {
			t.Fatalf("long single line preview lost truncation: %+v", got)
		}
	}
}

func TestPreviewTailByteLimitKeepsActualLineNumbers(t *testing.T) {
	got := PreviewTail("one\ntwo\nthree\nfour", 4, 10)
	if got.Text != "three\nfour" || !got.Truncated || got.StartLine != 3 || got.EndLine != 4 || got.NextLine != 3 {
		t.Fatalf("byte-limited tail has wrong source lines: %+v", got)
	}
}
