package llm_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/ww1489/seasprak/internal/llm"
)

type usageBody struct {
	data          []byte
	chunk         int
	reads, closes int
	end, closeErr error
}

func (b *usageBody) Read(p []byte) (int, error) {
	b.reads++
	if len(b.data) == 0 {
		return 0, b.end
	}
	n := min(len(p), len(b.data))
	if b.chunk > 0 {
		n = min(n, b.chunk)
	}
	copy(p, b.data[:n])
	b.data = b.data[n:]
	if len(b.data) == 0 {
		return n, b.end
	}
	return n, nil
}
func (b *usageBody) Close() error { b.closes++; return b.closeErr }

func TestP2UsageReadThroughJSONAndSSE(t *testing.T) {
	for _, tc := range []struct{ name, protocol, contentType, body string }{
		{"json", "anthropic-messages", "application/json", `{"content":[{"text":"中文🙂"}],"usage":{"input_tokens":300,"cache_read_input_tokens":600,"cache_creation_input_tokens":100,"output_tokens":50}}`},
		{"sse", "anthropic-messages", "text/event-stream", "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":300,\"cache_read_input_tokens\":600,\"cache_creation_input_tokens\":100,\"output_tokens\":0}}}\r\n\r\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":20}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":50}}\n\n"},
		{"sse-cr", "anthropic-messages", "text/event-stream", "data: {\"usage\":{\"input_tokens\":300,\"cache_read_input_tokens\":600,\"cache_creation_input_tokens\":100,\"output_tokens\":50}}\r\r"},
		{"deepseek", "deepseek-chat", "application/json", `{"usage":{"prompt_tokens":1000,"prompt_cache_hit_tokens":600,"prompt_cache_miss_tokens":400,"completion_tokens":50}}`},
	} {
		for _, chunk := range []int{1, 7, 4096} {
			t.Run(tc.name+string(rune('A'+chunk%20)), func(t *testing.T) {
				c := llm.NewUsageCollector(tc.protocol, 4096)
				base := &usageBody{data: []byte(tc.body), chunk: chunk, end: io.EOF}
				wrapped := llm.WrapUsageBody(base, tc.contentType, c)
				if base.reads != 0 {
					t.Fatal("collector preread body")
				}
				got, err := io.ReadAll(wrapped)
				if err != nil || !bytes.Equal(got, []byte(tc.body)) {
					t.Fatal("body bytes changed", err)
				}
				snapshot := c.Snapshot()
				if !snapshot.Complete || !snapshot.Usage.InputTotal.Known || snapshot.Usage.InputTotal.Value != 1000 || snapshot.Usage.CacheRead.Value != 600 || snapshot.Usage.OutputTotal.Value != 50 {
					t.Fatalf("incorrect usage: %+v", snapshot)
				}
				if tc.protocol == "anthropic-messages" && (!snapshot.Usage.CacheWrite.Known || snapshot.Usage.CacheWrite.Value != 100 || snapshot.Usage.UncachedInput.Value != 300) {
					t.Fatal("lost cache write presence")
				}
				if tc.protocol == "deepseek-chat" && snapshot.Usage.CacheWrite.Known {
					t.Fatal("invented cache writes")
				}
				if err = wrapped.Close(); err != nil || base.closes != 1 {
					t.Fatal("close not forwarded")
				}
			})
		}
	}
}

func TestP2UsageCloseDoesNotPretendEOF(t *testing.T) {
	base := &usageBody{data: []byte(`{"usage":{"prompt_tokens":7}}`), end: nil}
	c := llm.NewUsageCollector("openai-chat", 1024)
	r := llm.WrapUsageBody(base, "application/json", c)
	buf := make([]byte, 128)
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if c.Snapshot().Complete || base.reads != 1 {
		t.Fatal("Close fabricated EOF or performed another read")
	}
}

func TestP2UsagePreservesReadAndCloseErrors(t *testing.T) {
	end, closeErr := errors.New("synthetic read failure"), errors.New("synthetic close failure")
	body := []byte(`{"usage":{"prompt_tokens":12}}`)
	base := &usageBody{data: append([]byte(nil), body...), end: end, closeErr: closeErr}
	c := llm.NewUsageCollector("openai-chat", 1024)
	wrapped := llm.WrapUsageBody(base, "application/json", c)
	buf := make([]byte, 1024)
	n, err := wrapped.Read(buf)
	if n != len(body) || err != end || !bytes.Equal(buf[:n], body) {
		t.Fatal("Read tuple changed")
	}
	if wrapped.Close() != closeErr || base.closes != 1 {
		t.Fatal("Close error changed")
	}
	if c.Snapshot().Usage.InputTotal.Known {
		t.Fatal("failed body produced complete usage")
	}
}

func TestP2UsageUnknownMalformedAndOverflow(t *testing.T) {
	for _, tc := range []struct {
		protocol, ct, body string
		limit              int
	}{
		{"openai-chat", "application/json", `{"usage":{"prompt_tokens":0}}`, 1024},
		{"openai-chat", "application/json", `{"choices":[]}`, 1024},
		{"unknown", "application/json", `{"usage":{"prompt_tokens":100}}`, 1024},
		{"openai-chat", "text/plain", `{"usage":{"prompt_tokens":100}}`, 1024},
		{"openai-chat", "application/json", `{"usage":{"prompt_tokens":100}}`, 8},
		{"openai-chat", "application/json", `{"usage":{"prompt_tokens":100}}`, 0},
		{"openai-chat", "application/json", `{"usage":{"prompt_tokens":-1}}`, 1024},
		{"openai-chat", "text/event-stream", "data: {\"usage\":{\"prompt_tokens\":100}}\n\ndata: broken\n\n", 1024},
	} {
		c := llm.NewUsageCollector(tc.protocol, tc.limit)
		r := llm.WrapUsageBody(io.NopCloser(bytes.NewBufferString(tc.body)), tc.ct, c)
		got, err := io.ReadAll(r)
		if err != nil || string(got) != tc.body {
			t.Fatal("diagnostics corrupted body")
		}
		r.Close()
		s := c.Snapshot()
		if tc.body == `{"usage":{"prompt_tokens":0}}` {
			if !s.Usage.InputTotal.Known || s.Usage.InputTotal.Value != 0 {
				t.Fatal("known zero lost")
			}
		} else if s.Usage.InputTotal.Known {
			t.Fatal("unknown usage fabricated")
		}
	}
}
