package eino

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/testkit"
)

// Stream must use the same pairing assertion as Generate, rather than the
// embedded fake's Stream (which calls its own Generate).
func (m *p2BatchModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	if m.Calls() != 0 {
		seen := map[string]string{}
		for _, msg := range in {
			for _, b := range msg.ContentBlocks {
				if r := b.FunctionToolResult; r != nil {
					text := ""
					for _, c := range r.Content {
						if c.Text != nil {
							text += c.Text.Text
						}
					}
					seen[r.CallID+"/"+r.Name] = text
				}
			}
		}
		if len(seen) != 2 || seen["original-A/A"] != "A success" || seen["original-B/B"] != "B approved" {
			return nil, fmt.Errorf("incomplete tool pairing: %v", seen)
		}
	}
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

type p2StringStreamTool struct {
	name string
	run  func(context.Context) (*schema.StreamReader[string], error)
}

func (p p2StringStreamTool) Info(context.Context) (*schema.ToolInfo, error) {
	return testkit.ToolInfo(p.name, p.name), nil
}
func (p p2StringStreamTool) StreamableRun(ctx context.Context, _ string, _ ...tool.Option) (*schema.StreamReader[string], error) {
	return p.run(ctx)
}

type p2RichStreamTool struct{ p2StringStreamTool }

func (p p2RichStreamTool) StreamableRun(ctx context.Context, _ *schema.ToolArgument, _ ...tool.Option) (*schema.StreamReader[*schema.ToolResult], error) {
	r, err := p.run(ctx)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderWithConvert(r, func(s string) (*schema.ToolResult, error) { return p2TextResult(s), nil }), nil
}

// All producers are joined independently of TurnLoop.Wait. Reader EOF never
// waits for reader Close: the writer closes as soon as its work is complete.
func p2ProducedStream(t *testing.T, ctx context.Context, text string, failure error) *schema.StreamReader[string] {
	t.Helper()
	r, w := schema.Pipe[string](2)
	done := make(chan struct{})
	// At most two writes fit without a consumer. The framework exclusively
	// owns Close; native Pipe.Close is deliberately not idempotent.
	t.Cleanup(func() { p2Await(t, done, "stream producer cleanup") })
	go func() {
		defer close(done)
		defer w.Close()
		if text != "" && w.Send(text, nil) {
			return
		}
		if failure != nil {
			w.Send("", failure)
		}
	}()
	p2Await(t, done, "finite producer finished independently of Close")
	return r
}

func p2InstallStreamTools(t *testing.T, h *p2BatchProbe, enhanced, recvInterrupt bool) {
	h.streaming = true
	wrap := func(name string, run func(context.Context) (*schema.StreamReader[string], error)) tool.BaseTool {
		p := p2StringStreamTool{name: name, run: run}
		if enhanced {
			return p2RichStreamTool{p}
		}
		return p
	}
	h.toolsOverride = []tool.BaseTool{
		wrap("A", func(ctx context.Context) (*schema.StreamReader[string], error) {
			h.aCalls.Add(1)
			if compose.GetToolCallID(ctx) != "original-A" {
				return nil, errors.New("A identity changed")
			}
			r := p2ProducedStream(t, ctx, "A success", nil)
			h.aOnce.Do(func() { close(h.aDone) })
			return r, nil
		}),
		wrap("B", func(ctx context.Context) (*schema.StreamReader[string], error) {
			h.bCalls.Add(1)
			if compose.GetToolCallID(ctx) != "original-B" {
				return nil, errors.New("B identity changed")
			}
			select {
			case <-h.aDone:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			was, has, state := tool.GetInterruptState[string](ctx)
			if was && (!has || state != "original-B") {
				return nil, fmt.Errorf("lost B state: %t %q", has, state)
			}
			target, data, answer := tool.GetResumeContext[string](ctx)
			if was && target && data && answer == "approved" {
				h.bEffects.Add(1)
				return p2ProducedStream(t, ctx, "B approved", nil), nil
			}
			h.askOnce.Do(func() { close(h.askEntered) })
			select {
			case <-h.gate:
				err := tool.StatefulInterrupt(ctx, "approve B", "original-B")
				if recvInterrupt {
					return p2ProducedStream(t, ctx, "", err), nil
				}
				return nil, err
			case <-ctx.Done():
				h.cancelOnce.Do(func() { close(h.askCanceled) })
				return nil, ctx.Err()
			}
		}),
	}
}

func TestP2FrameworkStreamInterrupt(t *testing.T) {
	for _, enhanced := range []bool{false, true} {
		for _, recv := range []bool{false, true} {
			t.Run(fmt.Sprintf("enhanced_%t/recv_%t", enhanced, recv), func(t *testing.T) {
				h := newP2BatchProbe()
				p2InstallStreamTools(t, h, enhanced, recv)
				r := h.start(t, false, nil, false)
				if ok, _ := r.loop.Push(checkpointPrompt); !ok {
					t.Fatal("Push rejected")
				}
				p2Await(t, h.askEntered, "B asks")
				h.release()
				exit := r.wait(t)
				target := h.assertAskCheckpoint(t, exit, false)
				h.assertCounts(t, 1, 0, 1, 0)
				if recv {
					// Native v0.9.21 recognizes the nested interrupt, but loses
					// the successful sibling's cached result. This is a framework
					// limitation, not an acceptable product Resume implementation.
					unanswered := h.start(t, true, nil, false).wait(t)
					h.assertAskCheckpoint(t, unanswered, false)
					h.assertCounts(t, 2, 0, 1, 1)
					if h.bCalls.Load() != 2 || h.model.Calls() != 1 {
						t.Fatal("unexpected calls during native reader-error resume")
					}
					t.Log("native Recv StatefulInterrupt replays A: reject this recovery route; return the permission interrupt before opening the stream instead")
					// Deliberately never answer/reuse this unsafe checkpoint.
					// recv_false exercises the safe pre-stream alternative with
					// strict A=1 and B-effects=0/1 assertions at every resume.
					return
				}
				h.resumeAnswer(t, target)
			})
		}
	}
}

func TestP2FrameworkStreamGracefulAskCompetition(t *testing.T) {
	for _, enhanced := range []bool{false, true} {
		for _, afterAsk := range []bool{false, true} {
			t.Run(fmt.Sprintf("enhanced_%t/stop_after_ask_%t", enhanced, afterAsk), func(t *testing.T) {
				h := newP2BatchProbe()
				p2InstallStreamTools(t, h, enhanced, false)
				r := h.start(t, false, nil, afterAsk)
				r.loop.Push(checkpointPrompt)
				p2Await(t, h.askEntered, "stream B requests approval")
				if afterAsk {
					h.release()
					p2Await(t, r.askObserved, "stream interrupt observed")
				}
				r.loop.Stop(adk.WithGraceful())
				if !afterAsk {
					p2Await(t, r.ready, "event callback ready")
					p2Await(t, r.stopped, "graceful stop applied")
				}
				h.release()
				r.releaseEvents()
				target := h.assertAskCheckpoint(t, r.wait(t), !afterAsk)
				h.assertCounts(t, 1, 0, 1, 0)
				h.resumeAnswer(t, target)
			})
		}
	}
}

func TestP2FrameworkStreamAbort(t *testing.T) {
	for _, enhanced := range []bool{false, true} {
		t.Run(fmt.Sprintf("enhanced_%t", enhanced), func(t *testing.T) {
			h := newP2BatchProbe()
			p2InstallStreamTools(t, h, enhanced, false)
			r := h.start(t, false, nil, false)
			r.loop.Push(checkpointPrompt)
			p2Await(t, h.askEntered, "stream B starts")
			AbortTurnLoop(r.loop, r.cancel)
			p2Await(t, h.askCanceled, "stream B canceled")
			exit := r.wait(t)
			var ce *adk.CancelError
			if !errors.Is(exit.ExitReason, context.Canceled) && !errors.As(exit.ExitReason, &ce) {
				t.Fatalf("exit=%v", exit.ExitReason)
			}
			if exit.CheckpointAttempted {
				t.Fatal("abort published checkpoint")
			}
			h.assertCounts(t, 1, 0, 1, 0)
		})
	}
}

func TestP2FrameworkStreamAbortWaitsProducer(t *testing.T) {
	for _, enhanced := range []bool{false, true} {
		t.Run(fmt.Sprintf("enhanced_%t", enhanced), func(t *testing.T) {
			h := newP2BatchProbe()
			p2InstallStreamTools(t, h, enhanced, false)
			producerDone := make(chan struct{})
			p := p2StringStreamTool{name: "B", run: func(ctx context.Context) (*schema.StreamReader[string], error) {
				h.bCalls.Add(1)
				if compose.GetToolCallID(ctx) != "original-B" {
					return nil, errors.New("B identity changed")
				}
				select {
				case <-h.aDone:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				reader, writer := schema.Pipe[string](1)
				go func() {
					defer close(producerDone)
					defer writer.Close()
					close(h.askEntered)
					<-ctx.Done()
					close(h.askCanceled)
					// Model a producer that acknowledges cancellation but still has cleanup.
					<-h.gate
					writer.Send("", ctx.Err())
				}()
				return reader, nil
			}}
			if enhanced {
				h.toolsOverride[1] = p2RichStreamTool{p}
			} else {
				h.toolsOverride[1] = p
			}
			r := h.start(t, false, nil, false)
			t.Cleanup(func() { r.cancel(); h.release(); p2Await(t, producerDone, "abort producer cleanup") })
			r.loop.Push(checkpointPrompt)
			p2Await(t, h.askEntered, "reader producer started")
			AbortTurnLoop(r.loop, r.cancel)
			p2Await(t, h.askCanceled, "producer acknowledges cancellation")
			select {
			case <-r.done:
				t.Fatal("Wait returned before producer released its cleanup gate")
			default:
			}
			select {
			case <-producerDone:
				t.Fatal("producer falsely reported stopped")
			default:
			}
			h.release()
			p2Await(t, producerDone, "producer actually exited")
			exit := r.wait(t)
			if exit.CheckpointAttempted {
				t.Fatal("abort created resumable checkpoint")
			}
			h.assertCounts(t, 1, 0, 1, 0)
			if h.bCalls.Load() != 1 || h.model.Calls() != 1 {
				t.Fatal("abort repeated work")
			}
		})
	}
}

func TestP2FrameworkStreamProducerLifecycle(t *testing.T) {
	for _, enhanced := range []bool{false, true} {
		for _, mode := range []string{"eof", "close", "cancel"} {
			t.Run(fmt.Sprintf("enhanced_%t/%s", enhanced, mode), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				inner, w := schema.Pipe[string](0)
				started, done := make(chan struct{}), make(chan struct{})
				t.Cleanup(cancel)
				p := p2StringStreamTool{name: "probe", run: func(context.Context) (*schema.StreamReader[string], error) { return inner, nil }}
				var base tool.BaseTool = p
				if enhanced {
					base = p2RichStreamTool{p}
				}
				node, err := compose.NewAgenticToolsNode(ctx, &compose.ToolsNodeConfig{Tools: []tool.BaseTool{base}})
				if err != nil {
					t.Fatal(err)
				}
				reader, err := node.Stream(ctx, &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "lifecycle", Name: "probe", Arguments: `{}`})}})
				if err != nil {
					t.Fatal(err)
				}
				var once sync.Once
				closeReader := func() { once.Do(reader.Close) }
				stop := context.AfterFunc(ctx, closeReader)
				t.Cleanup(func() { cancel(); stop(); closeReader(); p2Await(t, done, "lifecycle producer") })
				go func() {
					defer close(done)
					defer w.Close()
					close(started)
					if mode == "eof" {
						w.Send("complete", nil)
						return
					}
					for !w.Send("chunk", nil) {
					}
				}()
				p2Await(t, started, "producer starts")
				if _, err := reader.Recv(); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "eof":
					if _, err := reader.Recv(); err != io.EOF {
						t.Fatalf("EOF=%v", err)
					}
					p2Await(t, done, "producer ended before reader Close")
				case "close":
					closeReader()
					p2Await(t, done, "Close releases producer")
				case "cancel":
					cancel()
					p2Await(t, done, "context releases producer")
				}
			})
		}
	}
}
