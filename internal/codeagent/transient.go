package codeagent

import (
	"cmp"
	"slices"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
)

// TransientModel is the latest public snapshot of one in-flight model stream.
// It replaces, never appends, and disappears once the attempt result commits.
type TransientModel struct {
	TraceID  string
	Snapshot agent.ModelStreamSnapshot
}

// TransientToolOutput is a bounded preview of one tool call stream. When the
// preview exceeds its limit only the newest bytes are kept and Truncated is set.
type TransientToolOutput struct {
	TraceID    string
	ToolCallID string
	Stream     string
	Text       string
	Truncated  bool
	order      uint64
}

// TransientView is display-only state recreated as empty after restart.
type TransientView struct {
	Models map[string]TransientModel      // keyed by stream ID
	Tools  map[string]TransientToolOutput // keyed by tool call ID + stream
}

type transientView struct {
	models map[string]TransientModel
	tools  map[string]*TransientToolOutput
	order  uint64
}

func (t *transientView) setModel(traceID string, s agent.ModelStreamSnapshot) {
	if t.models == nil {
		t.models = map[string]TransientModel{}
	}
	t.models[s.StreamID] = TransientModel{TraceID: traceID, Snapshot: s}
	t.bound()
}

func (t *transientView) dropModel(streamID string) { delete(t.models, streamID) }

func (t *transientView) appendTool(traceID string, d agent.ToolOutputDelta) {
	if t.tools == nil {
		t.tools = map[string]*TransientToolOutput{}
	}
	key := d.ToolCallID + "\x00" + d.Stream
	cur := t.tools[key]
	if cur == nil {
		cur = &TransientToolOutput{TraceID: traceID, ToolCallID: d.ToolCallID, Stream: d.Stream}
		t.tools[key] = cur
	}
	t.order++
	cur.order = t.order
	cur.Text += d.Text
	if len(cur.Text) > config.TransientToolPreviewBytes {
		cur.Text = keepTail(cur.Text, config.TransientToolPreviewBytes)
		cur.Truncated = true
	}
	t.bound()
}

// dropTrace clears every temporary stream of a trace after its terminal commit
// so late chunks cannot revive finished output.
func (t *transientView) dropTrace(traceID string) {
	for k, m := range t.models {
		if m.TraceID == traceID {
			delete(t.models, k)
		}
	}
	for k, o := range t.tools {
		if o.TraceID == traceID {
			delete(t.tools, k)
		}
	}
}

// bound enforces the session-wide byte budget by evicting the oldest tool
// previews first; model snapshots are small replacements and are kept.
func (t *transientView) bound() {
	total := 0
	for _, m := range t.models {
		for _, b := range m.Snapshot.Blocks {
			total += len(b.Text)
		}
	}
	keys := make([]string, 0, len(t.tools))
	for k, o := range t.tools {
		total += len(o.Text)
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return cmp.Compare(t.tools[a].order, t.tools[b].order) })
	for _, k := range keys {
		if total <= config.TransientSessionBytes {
			return
		}
		total -= len(t.tools[k].Text)
		delete(t.tools, k)
	}
}

func (t *transientView) copy() TransientView {
	out := TransientView{Models: map[string]TransientModel{}, Tools: map[string]TransientToolOutput{}}
	for k, m := range t.models {
		m.Snapshot.Blocks = append([]agent.ModelStreamBlock(nil), m.Snapshot.Blocks...)
		out.Models[k] = m
	}
	for k, o := range t.tools {
		out.Tools[k] = *o
	}
	return out
}

// keepTail returns at most n trailing bytes, starting at a UTF-8 boundary.
func keepTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
