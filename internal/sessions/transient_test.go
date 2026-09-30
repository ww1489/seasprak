package sessions

import (
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
)

func TestTransientViewReplacesBoundsAndClears(t *testing.T) {
	var v transientView
	v.setModel("t1", agent.ModelStreamSnapshot{StreamID: "m", ChunkSeq: 1, Blocks: []agent.ModelStreamBlock{{Type: "text", Text: "a"}}})
	v.setModel("t1", agent.ModelStreamSnapshot{StreamID: "m", ChunkSeq: 2, Blocks: []agent.ModelStreamBlock{{Type: "text", Text: "ab"}}})
	if got := v.copy().Models["m"]; got.Snapshot.ChunkSeq != 2 || len(got.Snapshot.Blocks) != 1 || got.Snapshot.Blocks[0].Text != "ab" {
		t.Fatalf("snapshot not replaced: %+v", got)
	}
	// Per-stream preview keeps the newest bytes at a UTF-8 boundary.
	big := strings.Repeat("中", config.TransientToolPreviewBytes)
	v.appendTool("t1", agent.ToolOutputDelta{ToolCallID: "c1", Stream: "stdout", Text: big})
	v.appendTool("t1", agent.ToolOutputDelta{ToolCallID: "c1", Stream: "stdout", Text: "END"})
	out := v.copy().Tools["c1\x00stdout"]
	if !out.Truncated || len(out.Text) > config.TransientToolPreviewBytes || !strings.HasSuffix(out.Text, "END") || !strings.HasPrefix(out.Text, "中") {
		t.Fatalf("preview bound broken: len=%d truncated=%v", len(out.Text), out.Truncated)
	}
	// Session total evicts the oldest previews first.
	for i := 0; i < 20; i++ {
		v.appendTool("t2", agent.ToolOutputDelta{ToolCallID: string(rune('A' + i)), Stream: "stdout", Text: strings.Repeat("x", config.TransientToolPreviewBytes)})
	}
	total := 0
	for _, o := range v.copy().Tools {
		total += len(o.Text)
	}
	if total > config.TransientSessionBytes {
		t.Fatalf("session total %d exceeds limit", total)
	}
	if _, kept := v.copy().Tools["c1\x00stdout"]; kept {
		t.Fatal("oldest preview was not evicted first")
	}
	// Separate streams of one call aggregate independently.
	v.appendTool("t3", agent.ToolOutputDelta{ToolCallID: "c9", Stream: "stdout", Text: "o"})
	v.appendTool("t3", agent.ToolOutputDelta{ToolCallID: "c9", Stream: "stderr", Text: "e"})
	if c := v.copy(); c.Tools["c9\x00stdout"].Text != "o" || c.Tools["c9\x00stderr"].Text != "e" {
		t.Fatal("streams merged")
	}
	v.dropTrace("t1")
	v.dropTrace("t3")
	if c := v.copy(); len(c.Models) != 0 || c.Tools["c9\x00stdout"].Text != "" {
		t.Fatal("terminal trace kept transient output")
	}
}
