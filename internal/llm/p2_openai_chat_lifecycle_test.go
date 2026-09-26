package llm_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

type p2ChatBlockingBody struct {
	ctx                  context.Context
	started              chan struct{}
	closed               chan struct{}
	startOnce, closeOnce sync.Once
	reads                atomic.Int32
}

func (b *p2ChatBlockingBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	b.startOnce.Do(func() { close(b.started) })
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.ErrClosedPipe
	}
}
func (b *p2ChatBlockingBody) Close() error { b.closeOnce.Do(func() { close(b.closed) }); return nil }

func TestP2OpenAIChatCloseInterruptsBlockedRead(t *testing.T) {
	c := llm.NewCatalog(nil)
	cfg := p2ChatConfig()
	var observed atomic.Int32
	body := &p2ChatBlockingBody{started: make(chan struct{}), closed: make(chan struct{})}
	p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		body.ctx = r.Context()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: r}, nil
	})}, 4096)
	p2OK(t, c.Register(cfg))
	m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, e)
	ctx, cancel := context.WithCancel(p2ChatContext(t, &observed))
	defer cancel()
	r, e := m.Stream(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
	p2OK(t, e)
	select {
	case <-body.started:
	case <-time.After(2 * time.Second):
		t.Fatal("adapter did not begin reading")
	}
	received := make(chan error, 1)
	go func() { _, err := r.Recv(); received <- err }()
	r.Close()
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Error("closing product reader did not cancel blocked HTTP read")
		cancel()
		<-body.closed
	}
	select {
	case err := <-received:
		if err == nil {
			t.Fatal("closed read reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("closed reader Recv remained blocked")
	}
	if observed.Load() != 1 {
		t.Fatal("request occupancy mismatch")
	}
}

type p2ChatFailBody struct {
	io.Reader
	closed atomic.Int32
}

func (b *p2ChatFailBody) Read(p []byte) (int, error) {
	n, e := b.Reader.Read(p)
	if e == io.EOF {
		return n, errors.New("synthetic-sensitive-body-error")
	}
	return n, e
}
func (b *p2ChatFailBody) Close() error { b.closed.Add(1); return nil }
func TestP2OpenAIChatBrokenStreamAndCancel(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "read_failure", true: "cancelled"}[cancelled], func(t *testing.T) {
			c := llm.NewCatalog(nil)
			cfg := p2ChatConfig()
			var observed, physical atomic.Int32
			body := &p2ChatFailBody{Reader: strings.NewReader("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")}
			p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				physical.Add(1)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: r}, nil
			})}, 4096)
			p2OK(t, c.Register(cfg))
			m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, e)
			ctx, cancel := context.WithCancel(p2ChatContext(t, &observed))
			defer cancel()
			if cancelled {
				cancel()
			}
			_, e = p2ChatInvoke(ctx, m, true, []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
			if cancelled {
				if !errors.Is(e, context.Canceled) || physical.Load() != 0 || observed.Load() != 0 {
					t.Fatal("cancelled request was sent")
				}
			} else {
				p2Code(t, e, product.CodeResourceUnavailable)
				if strings.Contains(e.Error(), "synthetic-sensitive") {
					t.Fatal("body error leaked")
				}
				if physical.Load() != 1 || observed.Load() != 1 || body.closed.Load() != 1 {
					t.Fatal("failed response did not close exactly once")
				}
			}
		})
	}
}

func TestP2OpenAIChatHTTPFailureClosesBody(t *testing.T) {
	c := llm.NewCatalog(nil)
	cfg := p2ChatConfig()
	var observed atomic.Int32
	body := &p2ChatFailBody{Reader: strings.NewReader(`{"error":{"message":"fixture"}}`)}
	p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: body, Request: r}, nil
	})}, 4096)
	p2OK(t, c.Register(cfg))
	m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, e)
	_, e = m.Stream(p2ChatContext(t, &observed), []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
	p2Code(t, e, product.CodeResourceUnavailable)
	if body.closed.Load() != 1 {
		t.Fatal("failed stream establishment leaked response body")
	}
}

func TestP2OpenAIChatRedirectDisabled(t *testing.T) {
	c := llm.NewCatalog(nil)
	cfg := p2ChatConfig()
	var physical, observed atomic.Int32
	client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		physical.Add(1)
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": {"https://other.invalid/v1"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	p2ChatRegister(t, c, client, 4096)
	if client.CheckRedirect != nil {
		t.Fatal("caller client mutated")
	}
	p2OK(t, c.Register(cfg))
	m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, e)
	_, e = m.Generate(p2ChatContext(t, &observed), []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
	if e == nil || physical.Load() != 1 || observed.Load() != 1 {
		t.Fatal("redirect introduced an unapproved request")
	}
}
