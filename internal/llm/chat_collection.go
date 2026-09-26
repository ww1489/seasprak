package llm

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
)

// The existing collector owns framing, locking and body consumption. Refusal
// has a separate total bound across all frames and is released only after
// credential redaction. No other response prose enters diagnostics or usage.
type chatCollection struct {
	refused, invalid, seen, done, complete bool
	refusal                                string
	refusalOverflow                        bool
	body                                   io.Closer
}

const maxRefusalBytes = 4096

func (c *chatCollection) collectRefusal(text *string) {
	if text == nil || *text == "" {
		return
	}
	c.refused = true
	if c.refusalOverflow {
		return
	}
	if len(*text) > maxRefusalBytes-len(c.refusal) {
		// Do not expose a prefix: it could end inside the current credential.
		c.refusal, c.refusalOverflow = "", true
		return
	}
	c.refusal += *text
}

type chatCollectorKey struct{}

// Chat's SDK omits Close on failed stream establishment. Keep an idempotent
// close handle so both the adapter and request owner can release the same body.
type chatResponseBody struct {
	io.ReadCloser
	collector *UsageCollector
	once      sync.Once
	err       error
}

func (b *chatResponseBody) Close() error {
	b.once.Do(func() {
		b.err = b.ReadCloser.Close()
		if b.err != nil {
			b.collector.mu.Lock()
			b.collector.chat.invalid = true
			b.collector.mu.Unlock()
		}
	})
	return b.err
}
func (c *UsageCollector) closeChatBody() {
	c.mu.Lock()
	var body io.Closer
	if c.chat != nil {
		body = c.chat.body
	}
	c.mu.Unlock()
	if body != nil {
		_ = body.Close()
	}
}

func (c *UsageCollector) collectChat(data []byte) {
	var wire struct {
		Choices *[]struct {
			Index   *int `json:"index"`
			Message *struct {
				Refusal *string `json:"refusal"`
			} `json:"message"`
			Delta *struct {
				Refusal *string `json:"refusal"`
			} `json:"delta"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &wire) != nil || wire.Choices == nil {
		c.chat.invalid = true
		return
	}
	if len(*wire.Choices) == 0 {
		if c.format != "sse" || len(wire.Usage) == 0 || bytes.Equal(wire.Usage, []byte("null")) {
			c.chat.invalid = true
		}
		return
	}
	if len(*wire.Choices) != 1 {
		c.chat.invalid = true
		return
	}
	choice := (*wire.Choices)[0]
	if choice.Index == nil || *choice.Index != 0 {
		c.chat.invalid = true
		return
	}
	if c.format == "json" {
		if choice.Message == nil {
			c.chat.invalid = true
			return
		}
		c.chat.collectRefusal(choice.Message.Refusal)
	} else {
		if choice.Delta == nil {
			c.chat.invalid = true
			return
		}
		c.chat.collectRefusal(choice.Delta.Refusal)
	}
	c.chat.seen = true
}
func (c *UsageCollector) completeChat() {
	if c.chat == nil {
		return
	}
	c.chat.complete = !c.chat.invalid && !c.disabled && c.chat.seen && ((c.format == "json" && json.Valid(c.buffer)) || (c.format == "sse" && c.chat.done && len(bytes.TrimSpace(c.buffer)) == 0))
}
func (c *UsageCollector) chatResult() (bool, UsageRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.chat == nil || !c.chat.complete || c.chat.invalid || c.disabled {
		return false, UsageRecord{}, invalid("incomplete Chat response metadata")
	}
	return c.chat.refused, c.snapshot.Usage, nil
}
