package llm

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/ww1489/seasprak/internal/llm/einoext/agenticgemini"
	"google.golang.org/genai"
)

type geminiCollectorKey struct{}

type geminiRawCall struct {
	candidate, part int
	id, name        string
	args            json.RawMessage
}
type geminiRawFrame struct {
	parts   []int
	indexes []int32
	calls   []geminiRawCall
	size    int
}

// Protected by UsageCollector.mu. The queued records, including structural
// overhead, share a cumulative limit separate from the existing frame buffer.
// Raw arguments never enter UsageSnapshot, diagnostics or public Extra fields.
type geminiArgumentCollection struct {
	frames          []geminiRawFrame
	bytes           int
	invalid, closed bool
	exhausted       bool // Capacity loss permits text-only statistics degradation.
	bound           bool
}

type geminiResponseRestorer struct {
	collector *UsageCollector
	ctx       context.Context
	stop      func() bool
}

func newGeminiResponseRestorer(ctx context.Context, limit int) (context.Context, agenticgemini.ResponseRestorer) {
	c := NewUsageCollector("gemini-generate-content", limit)
	c.gemini = &geminiArgumentCollection{invalid: limit <= 0}
	r := &geminiResponseRestorer{collector: c, ctx: ctx}
	c.mu.Lock()
	r.stop = context.AfterFunc(ctx, r.Close)
	c.mu.Unlock()
	return context.WithValue(ctx, geminiCollectorKey{}, c), r
}

func (c *UsageCollector) collectGeminiArguments(data []byte) {
	g := c.gemini
	if g.invalid || g.closed {
		return
	}
	var wire struct {
		Candidates []*struct {
			Index   int32 `json:"index"`
			Content *struct {
				Parts []*struct {
					Call *struct {
						ID   string          `json:"id"`
						Name string          `json:"name"`
						Args json.RawMessage `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(data, &wire) != nil {
		g.invalid = true
		return
	}
	// Conservative charges include slice growth capacity, not just payload
	// length (frame <= 80 bytes, candidate slots <= 12, call <= 72 on 64-bit).
	frame := geminiRawFrame{size: 192 + len(wire.Candidates)*32}
	for i, candidate := range wire.Candidates {
		if candidate == nil {
			g.invalid = true
			return
		}
		frame.indexes = append(frame.indexes, candidate.Index)
		parts := -1
		if candidate.Content != nil {
			parts = len(candidate.Content.Parts)
			for j, part := range candidate.Content.Parts {
				if part == nil {
					g.invalid = true
					return
				}
				if call := part.Call; call != nil {
					frame.size += 160 + len(call.ID) + len(call.Name) + len(call.Args)
					frame.calls = append(frame.calls, geminiRawCall{candidate: i, part: j, id: call.ID, name: call.Name, args: call.Args})
				}
			}
		}
		frame.parts = append(frame.parts, parts)
	}
	if frame.size > c.limit-g.bytes {
		g.invalid = true
		g.exhausted = true
		return
	}
	g.bytes += frame.size
	g.frames = append(g.frames, frame)
}

func (r *geminiResponseRestorer) Restore(resp *genai.GenerateContentResponse) error {
	c := r.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := r.ctx.Err(); err != nil {
		return err
	}
	g := c.gemini
	fail := func() error {
		g.invalid = true
		return invalid("invalid Gemini function argument association")
	}
	if g.closed || !g.bound || resp == nil {
		return fail()
	}
	if g.invalid {
		// The usage buffer limit has always degraded statistics, not ordinary
		// text responses. With capture lost, permit only responses that have no
		// arguments to restore; this collection remains invalid for later calls.
		if g.exhausted {
			for _, candidate := range resp.Candidates {
				if candidate == nil || candidate.Content == nil {
					continue
				}
				for _, part := range candidate.Content.Parts {
					if part != nil && part.FunctionCall != nil {
						return fail()
					}
				}
			}
			return nil
		}
		return fail()
	}
	if len(g.frames) == 0 {
		return fail()
	}
	frame := g.frames[0]
	if len(resp.Candidates) != len(frame.parts) {
		return fail()
	}
	calls := 0
	for i, candidate := range resp.Candidates {
		if candidate == nil || candidate.Index != frame.indexes[i] {
			return fail()
		}
		if candidate.Content == nil {
			if frame.parts[i] != -1 {
				return fail()
			}
			continue
		}
		if len(candidate.Content.Parts) != frame.parts[i] {
			return fail()
		}
		for j, part := range candidate.Content.Parts {
			if part == nil {
				return fail()
			}
			if call := part.FunctionCall; call != nil {
				if calls >= len(frame.calls) {
					return fail()
				}
				raw := frame.calls[calls]
				if raw.candidate != i || raw.part != j || raw.id != call.ID || raw.name != call.Name {
					return fail()
				}
				calls++
			}
		}
	}
	if calls != len(frame.calls) {
		return fail()
	}
	// Decode and validate all arguments before changing any provider object.
	args := make([]map[string]any, len(frame.calls))
	for i, raw := range frame.calls {
		if len(raw.args) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(raw.args))
		decoder.UseNumber()
		if decoder.Decode(&args[i]) != nil {
			return fail()
		}
	}
	for i, raw := range frame.calls {
		resp.Candidates[raw.candidate].Content.Parts[raw.part].FunctionCall.Args = args[i]
	}
	g.bytes -= frame.size
	g.frames[0] = geminiRawFrame{}
	g.frames = g.frames[1:]
	if len(g.frames) == 0 {
		g.frames = nil
	}
	return nil
}

func (r *geminiResponseRestorer) Close() {
	c := r.collector
	c.mu.Lock()
	g := c.gemini
	g.closed = true
	clear(g.frames)
	g.frames = nil
	g.bytes = 0
	clear(c.buffer)
	c.buffer = nil
	stop := r.stop
	r.stop = nil
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
}
