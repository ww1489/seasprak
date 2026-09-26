package llm

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type p2ChatTupleBody struct {
	data          []byte
	end, closeErr error
	reads, closes int
}

func (b *p2ChatTupleBody) Read(p []byte) (int, error) {
	b.reads++
	n := copy(p, b.data)
	b.data = b.data[n:]
	if len(b.data) == 0 {
		return n, b.end
	}
	return n, nil
}
func (b *p2ChatTupleBody) Close() error { b.closes++; return b.closeErr }

func TestP2OpenAIChatReadThroughSemantics(t *testing.T) {
	for _, chunk := range []int{1, 7, 4096} {
		payload := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"refusal\":\"中文😀\"}}]}\r\n\r\ndata: [DONE]\r\n\r\n")
		body := &p2ChatTupleBody{data: append([]byte(nil), payload...), end: io.EOF}
		collector := NewUsageCollector("openai-chat", 4096)
		collector.chat = &chatCollection{}
		reader := WrapUsageBody(body, "text/event-stream", collector)
		if body.reads != 0 {
			t.Fatal("collection pre-read response")
		}
		var actual []byte
		buffer := make([]byte, chunk)
		for {
			n, e := reader.Read(buffer)
			actual = append(actual, buffer[:n]...)
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatal(e)
			}
		}
		if !bytes.Equal(actual, payload) {
			t.Fatal("read-through modified bytes")
		}
		refused, _, e := collector.chatResult()
		if e != nil || !refused {
			t.Fatal("SSE framing lost refusal")
		}
		if e = reader.Close(); e != nil || body.closes != 1 {
			t.Fatal("close not forwarded")
		}
		if bytes.Contains([]byte(collector.Snapshot().Diagnostic), []byte("中文")) {
			t.Fatal("refusal content retained in diagnostic")
		}
	}
	end, closeErr := errors.New("read fixture"), errors.New("close fixture")
	body := &p2ChatTupleBody{data: []byte(`{"choices":[{"index":0,"message":{"refusal":null}}]}`), end: end, closeErr: closeErr}
	c := NewUsageCollector("openai-chat", 4096)
	c.chat = &chatCollection{}
	r := WrapUsageBody(body, "application/json", c)
	p := make([]byte, 4096)
	want := append([]byte(nil), body.data...)
	n, e := r.Read(p)
	if e != end || !bytes.Equal(p[:n], want) {
		t.Fatal("read tuple changed")
	}
	if r.Close() != closeErr || body.closes != 1 {
		t.Fatal("close result changed")
	}
	if _, _, e = c.chatResult(); e == nil {
		t.Fatal("read failure accepted")
	}
}

func TestP2OpenAIChatCollectorRejectsEarlyClose(t *testing.T) {
	c := NewUsageCollector("openai-chat", 4096)
	c.chat = &chatCollection{}
	body := &p2ChatTupleBody{data: []byte(`{"choices":[{"index":0,"message":{"refusal":null}}]}`), end: io.EOF}
	r := WrapUsageBody(body, "application/json", c)
	p := make([]byte, 8)
	_, _ = r.Read(p)
	_ = r.Close()
	if _, _, e := c.chatResult(); e == nil {
		t.Fatal("partial JSON close accepted")
	}
	if body.reads != 1 {
		t.Fatal("close drained the body")
	}
}
