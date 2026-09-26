package eino

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/testkit"
)

// Framework probes only: product authorization and final settlement for these
// interfaces remain Step 11, and must not be inferred from these tests.
type p2ToolProbe struct {
	mu     sync.Mutex
	args   string
	callID string
	calls  int
}

func (p *p2ToolProbe) Info(context.Context) (*schema.ToolInfo, error) {
	return testkit.ToolInfo("probe", "framework probe"), nil
}
func (p *p2ToolProbe) note(ctx context.Context, args string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.args, p.callID = args, compose.GetToolCallID(ctx)
	p.calls++
}

type p2InvokeProbe struct{ *p2ToolProbe }

func (p p2InvokeProbe) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	p.note(ctx, args)
	return "first second", nil
}

type p2StreamProbe struct{ *p2ToolProbe }

func (p p2StreamProbe) StreamableRun(ctx context.Context, args string, _ ...tool.Option) (*schema.StreamReader[string], error) {
	p.note(ctx, args)
	return schema.StreamReaderFromArray([]string{"first ", "second"}), nil
}

type p2EnhancedProbe struct{ *p2ToolProbe }

func (p p2EnhancedProbe) InvokableRun(ctx context.Context, args *schema.ToolArgument, _ ...tool.Option) (*schema.ToolResult, error) {
	p.note(ctx, args.Text)
	return p2TextResult("first second"), nil
}

type p2EnhancedStreamProbe struct{ *p2ToolProbe }

func (p p2EnhancedStreamProbe) StreamableRun(ctx context.Context, args *schema.ToolArgument, _ ...tool.Option) (*schema.StreamReader[*schema.ToolResult], error) {
	p.note(ctx, args.Text)
	return schema.StreamReaderFromArray([]*schema.ToolResult{p2TextResult("first "), p2TextResult("second")}), nil
}
func p2TextResult(text string) *schema.ToolResult {
	return &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: text}}}
}

type p2OpenStreamProbe struct {
	*p2ToolProbe
	reader *schema.StreamReader[string]
}

func (p p2OpenStreamProbe) StreamableRun(ctx context.Context, args string, _ ...tool.Option) (*schema.StreamReader[string], error) {
	p.note(ctx, args)
	return p.reader, nil
}

func TestP2FrameworkToolStreamClosePropagatesToProducer(t *testing.T) {
	inner, writer := schema.Pipe[string](0)
	probe := &p2ToolProbe{}
	node, err := compose.NewAgenticToolsNode(t.Context(), &compose.ToolsNodeConfig{Tools: []tool.BaseTool{p2OpenStreamProbe{probe, inner}}})
	if err != nil {
		t.Fatal(err)
	}
	input := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{
		schema.NewContentBlock(&schema.FunctionToolCall{CallID: "stream-call", Name: "probe", Arguments: `{}`}),
	}}
	reader, err := node.Stream(t.Context(), input)
	if err != nil {
		writer.Close()
		inner.Close()
		t.Fatal(err)
	}
	var closeOnce sync.Once
	closedReader := make(chan struct{})
	closeReader := func() { closeOnce.Do(func() { reader.Close(); close(closedReader) }) }
	done := make(chan bool, 1)
	go func() {
		defer writer.Close()
		if writer.Send("first", nil) {
			done <- false
			return
		}
		// v0.9.21 merges converted readers through an asynchronous toStream
		// pump. Close closes the pump output, not the source immediately.
		// One pending recv may still accept a chunk; its next output send
		// observes Close and the deferred source Close rejects the next send.
		// This proves eventual propagation, not that Close joins the producer.
		<-closedReader
		if writer.Send("pending prefetch", nil) {
			done <- true
			return
		}
		done <- writer.Send("must be rejected after pending prefetch", nil)
	}()
	t.Cleanup(func() {
		closeReader()
		select {
		case closed := <-done:
			if !closed {
				t.Error("producer was not released by reader Close")
			}
		case <-time.After(5 * time.Second):
			t.Error("framework failed to release stream producer")
		}
	})
	if _, err := reader.Recv(); err != nil {
		t.Fatal(err)
	}
	closeReader()
}

func TestP2FrameworkToolInterfaces(t *testing.T) {
	factories := []struct {
		name string
		make func(*p2ToolProbe) tool.BaseTool
	}{
		{"invokable", func(p *p2ToolProbe) tool.BaseTool { return p2InvokeProbe{p} }},
		{"streamable", func(p *p2ToolProbe) tool.BaseTool { return p2StreamProbe{p} }},
		{"enhanced", func(p *p2ToolProbe) tool.BaseTool { return p2EnhancedProbe{p} }},
		{"enhanced_streamable", func(p *p2ToolProbe) tool.BaseTool { return p2EnhancedStreamProbe{p} }},
	}
	for _, factory := range factories {
		for _, streamed := range []bool{false, true} {
			mode := "invoke"
			if streamed {
				mode = "stream"
			}
			t.Run(factory.name+"/"+mode, func(t *testing.T) {
				probe := &p2ToolProbe{}
				node, err := compose.NewAgenticToolsNode(t.Context(), &compose.ToolsNodeConfig{Tools: []tool.BaseTool{factory.make(probe)}})
				if err != nil {
					t.Fatal(err)
				}
				const arguments = `{"n":9007199254740993,"text":"中文"}`
				input := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{
					schema.NewContentBlock(&schema.FunctionToolCall{CallID: "original-call", Name: "probe", Arguments: arguments}),
				}}
				var result []*schema.AgenticMessage
				if !streamed {
					result, err = node.Invoke(t.Context(), input)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					reader, err := node.Stream(t.Context(), input)
					if err != nil {
						t.Fatal(err)
					}
					defer reader.Close()
					for {
						chunk, recvErr := reader.Recv()
						if recvErr == io.EOF {
							break
						}
						if recvErr != nil {
							t.Fatal(recvErr)
						}
						result = append(result, chunk...)
					}
				}
				var text string
				for _, msg := range result {
					if msg == nil {
						continue
					}
					for _, block := range msg.ContentBlocks {
						if block == nil || block.FunctionToolResult == nil {
							t.Fatal("missing tool result")
						}
						call := block.FunctionToolResult
						if call.CallID != "original-call" || call.Name != "probe" {
							t.Fatalf("identity changed: %+v", call)
						}
						for _, content := range call.Content {
							if content.Text != nil {
								text += content.Text.Text
							}
						}
					}
				}
				if text != "first second" {
					t.Fatalf("result=%q", text)
				}
				probe.mu.Lock()
				defer probe.mu.Unlock()
				if probe.calls != 1 || probe.args != arguments || probe.callID != "original-call" {
					t.Fatalf("calls=%d args=%q id=%q", probe.calls, probe.args, probe.callID)
				}
			})
		}
	}
}
