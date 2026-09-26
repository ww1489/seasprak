package llm

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"mime"
	"sync"
)

// UsageSnapshot contains only normalized measurements and fixed diagnostics.
// Complete refers to body consumption, not model finish/response acceptance.
type UsageSnapshot struct {
	Usage      UsageRecord
	Diagnostic string
	Complete   bool
}

// UsageCollector belongs to exactly one HTTP response. It retains at most one
// bounded JSON document/SSE frame in memory; raw bytes never leave this object.
type UsageCollector struct {
	mu         sync.Mutex
	protocol   string
	limit      int
	format     string
	buffer     []byte
	snapshot   UsageSnapshot
	disabled   bool
	ended      bool
	previousCR bool
	chat       *chatCollection // Optional fail-closed Chat metadata, under mu.
}

// NewUsageCollector requires an explicit positive buffer limit supplied by
// trusted assembly. An invalid limit disables diagnostics, never body reads.
func NewUsageCollector(protocol string, maxBytes int) *UsageCollector {
	c := &UsageCollector{protocol: protocol, limit: maxBytes}
	if maxBytes <= 0 {
		c.disable("invalid_usage_buffer_limit")
	}
	return c
}
func (c *UsageCollector) Snapshot() UsageSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot
}

// WrapUsageBody does not read until its caller reads, and returns each original
// Read tuple and Close error unchanged. The caller remains the sole consumer.
func WrapUsageBody(body io.ReadCloser, contentType string, c *UsageCollector) io.ReadCloser {
	c.mu.Lock()
	defer c.mu.Unlock()
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		c.disable("unsupported_content_type")
	} else {
		switch media {
		case "application/json":
			c.format = "json"
		case "text/event-stream":
			c.format = "sse"
		default:
			c.disable("unsupported_content_type")
		}
	}
	switch c.protocol {
	case "anthropic-messages", "openai-chat", "openai-responses", "deepseek-chat", "gemini-generate-content":
	default:
		c.disable("unsupported_usage_protocol")
	}
	return &usageReadCloser{body: body, collector: c}
}

type usageReadCloser struct {
	body      io.ReadCloser
	collector *UsageCollector
	notify    func(UsageSnapshot)
	reported  sync.Once
}

func (b *usageReadCloser) report() {
	b.reported.Do(func() {
		if b.notify != nil {
			b.notify(b.collector.Snapshot())
		}
	})
}
func (b *usageReadCloser) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	b.collector.consume(p[:n], err)
	if err != nil {
		b.report()
	}
	return n, err
}
func (b *usageReadCloser) Close() error {
	err := b.body.Close()
	b.collector.close()
	b.report()
	return err
}
func (c *UsageCollector) disable(reason string) {
	if c.chat != nil {
		c.chat.invalid = true
	}
	clear(c.buffer)
	c.buffer = nil
	c.snapshot = UsageSnapshot{Diagnostic: reason}
	c.disabled = true
}
func (c *UsageCollector) consume(data []byte, readErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled || c.ended {
		return
	}
	// Byte-wise framing avoids a transient allocation proportional to Read size.
	for _, b := range data {
		if c.format == "sse" {
			if b == '\n' && c.previousCR {
				c.previousCR = false
				continue
			}
			c.previousCR = b == '\r'
			if b == '\r' {
				b = '\n'
			}
		}
		if len(c.buffer) >= c.limit {
			c.disable("usage_buffer_limit")
			break
		}
		c.buffer = append(c.buffer, b)
		if c.format == "sse" && b == '\n' {
			n := len(c.buffer)
			if n >= 2 && c.buffer[n-2] == '\n' {
				c.parseSSE(c.buffer)
				clear(c.buffer)
				c.buffer = c.buffer[:0]
				if c.disabled {
					break
				}
			}
		}
	}
	if c.disabled {
		return
	}
	if readErr != nil {
		if readErr != io.EOF {
			c.disable("usage_read_failed")
			return
		}
		c.finish()
	}
}
func (c *UsageCollector) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled || c.ended {
		return
	}
	// JSON decoders can stop at a complete value without asking for EOF.
	// Retain observed usage, but Close is not evidence that the body ended.
	if c.format == "json" && json.Valid(c.buffer) {
		c.parseJSON(c.buffer)
	}
	c.completeChat()
	clear(c.buffer)
	c.buffer = nil
	c.ended = true
	if !c.disabled {
		c.snapshot.Diagnostic = "body_closed_before_eof"
	}
}
func (c *UsageCollector) finish() {
	if c.format == "json" {
		c.parseJSON(c.buffer)
	} else if len(bytes.TrimSpace(c.buffer)) != 0 {
		c.disable("incomplete_sse_frame")
	}
	c.completeChat()
	clear(c.buffer)
	c.buffer = nil
	c.ended = true
	if !c.disabled {
		c.snapshot.Complete = true
		if c.snapshot.Usage == (UsageRecord{}) {
			c.snapshot.Diagnostic = "usage_not_reported"
		}
	}
}
func (c *UsageCollector) parseSSE(frame []byte) {
	var data []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimPrefix(line, []byte("data:"))
			value = bytes.TrimPrefix(value, []byte(" "))
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
	}
	if len(data) == 0 {
		return
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		if c.chat != nil {
			c.chat.done = true
		}
		return
	}
	if c.chat != nil && c.chat.done {
		c.chat.invalid = true
	}
	c.parseJSON(data)
	clear(data)
}

// Only the usage objects are decoded into typed fields. Other response content
// is skipped by encoding/json and is never retained in snapshots or errors.
type wireUsage struct {
	Prompt        *int64 `json:"prompt_tokens"`
	Completion    *int64 `json:"completion_tokens"`
	Input         *int64 `json:"input_tokens"`
	Output        *int64 `json:"output_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
	CacheWrite    *int64 `json:"cache_creation_input_tokens"`
	CacheCreation struct {
		Five *int64 `json:"ephemeral_5m_input_tokens"`
		Hour *int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	PromptDetails struct {
		Cached *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails struct {
		Reasoning *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	InputDetails struct {
		Cached *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		Reasoning *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	Hit  *int64 `json:"prompt_cache_hit_tokens"`
	Miss *int64 `json:"prompt_cache_miss_tokens"`
}
type geminiUsage struct {
	Prompt     *int64 `json:"promptTokenCount"`
	Candidates *int64 `json:"candidatesTokenCount"`
	Thoughts   *int64 `json:"thoughtsTokenCount"`
	Cached     *int64 `json:"cachedContentTokenCount"`
}

func (c *UsageCollector) parseJSON(data []byte) {
	if c.chat != nil {
		c.collectChat(data)
	}
	var envelope struct {
		Type    string     `json:"type"`
		Usage   *wireUsage `json:"usage"`
		Message struct {
			Usage *wireUsage `json:"usage"`
		} `json:"message"`
		Response struct {
			Usage *wireUsage `json:"usage"`
		} `json:"response"`
		Gemini *geminiUsage `json:"usageMetadata"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		c.disable("invalid_usage_frame")
		return
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		c.disable("unsupported_usage_format")
		return
	}
	u := envelope.Usage
	if c.protocol == "anthropic-messages" && envelope.Type == "message_start" {
		u = envelope.Message.Usage
	}
	if c.protocol == "openai-responses" && envelope.Type != "" {
		u = envelope.Response.Usage
	}
	next := UsageRecord{}
	set := func(dst *UsageValue, value *int64) {
		if value != nil {
			*dst = UsageValue{Value: *value, Known: true, Source: c.protocol + "/raw_usage"}
		}
	}
	if c.protocol == "gemini-generate-content" {
		if envelope.Gemini == nil {
			return
		}
		g := envelope.Gemini
		set(&next.InputTotal, g.Prompt)
		set(&next.CacheRead, g.Cached)
		set(&next.Reasoning, g.Thoughts)
		// Candidate and thinking token counts are disjoint. Without both fields a
		// total output value cannot be asserted from their SDK default zero values.
		if g.Candidates != nil && g.Thoughts != nil && *g.Candidates >= 0 && *g.Thoughts >= 0 && *g.Candidates <= math.MaxInt64-*g.Thoughts {
			n := *g.Candidates + *g.Thoughts
			set(&next.OutputTotal, &n)
		}
	} else {
		if u == nil {
			return
		}
		switch c.protocol {
		case "anthropic-messages":
			set(&next.UncachedInput, u.Input)
			set(&next.CacheRead, u.CacheRead)
			set(&next.CacheWrite, u.CacheWrite)
			set(&next.OutputTotal, u.Output)
			set(&next.CacheWrite5Minutes, u.CacheCreation.Five)
			set(&next.CacheWrite1Hour, u.CacheCreation.Hour)
		case "openai-chat", "deepseek-chat":
			set(&next.InputTotal, u.Prompt)
			set(&next.OutputTotal, u.Completion)
			set(&next.CacheRead, u.PromptDetails.Cached)
			set(&next.Reasoning, u.CompletionDetails.Reasoning)
			if c.protocol == "deepseek-chat" {
				set(&next.CacheRead, u.Hit)
				set(&next.UncachedInput, u.Miss)
			}
		case "openai-responses":
			set(&next.InputTotal, u.Input)
			set(&next.OutputTotal, u.Output)
			set(&next.CacheRead, u.InputDetails.Cached)
			set(&next.Reasoning, u.OutputDetails.Reasoning)
		}
	}
	merged, err := c.snapshot.Usage.MergeCumulative(next)
	if err != nil {
		c.disable("invalid_usage_measurement")
		return
	}
	if c.protocol == "anthropic-messages" && merged.UncachedInput.Known && merged.CacheRead.Known && merged.CacheWrite.Known {
		a, b, d := merged.UncachedInput.Value, merged.CacheRead.Value, merged.CacheWrite.Value
		if a > math.MaxInt64-b || a+b > math.MaxInt64-d {
			c.disable("invalid_usage_measurement")
			return
		}
		merged.InputTotal = UsageValue{Value: a + b + d, Known: true, Source: c.protocol + "/raw_usage"}
	}
	c.snapshot.Usage = merged
}
