package tools

import (
	"context"
	"encoding/json"
	"sync"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

// callOutput serializes one claimed call's temporary output with its close.
// CommitFact acknowledges mailbox publication before another chunk is accepted.
type callOutput struct {
	mu       sync.Mutex
	runCtx   context.Context
	sink     agent.ExecutionSink
	scope    agent.ExecutionScope
	callID   string
	streamID string
	seq      uint64
	closed   bool
}

func (o *callOutput) WriteOutput(ctx context.Context, chunk agent.ToolOutputChunk) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return product.NewError(product.CodeStateConflict, "tool output is closed")
	}
	if err := o.runCtx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stream := chunk.Stream
	if stream == "" {
		stream = "output"
	}
	if stream != "output" && stream != "stdout" && stream != "stderr" {
		return product.NewError(product.CodeInvalidArgument, "tool output stream is invalid")
	}
	if !utf8.ValidString(chunk.Text) {
		return product.NewError(product.CodeInvalidArgument, "tool output text is not valid UTF-8")
	}
	if chunk.Text == "" {
		return nil
	}
	for start := 0; start < len(chunk.Text); {
		end := start + config.ToolOutputChunkBytes
		if end >= len(chunk.Text) {
			end = len(chunk.Text)
		} else {
			for !utf8.RuneStart(chunk.Text[end]) {
				end--
			}
		}
		update := agent.ToolOutputFact{
			ToolOutputDelta: agent.ToolOutputDelta{ToolCallID: o.callID, Stream: stream, Text: chunk.Text[start:end]},
			CallID:          o.callID, StreamID: o.streamID, ChunkSeq: o.seq + 1,
		}
		raw, err := json.Marshal(update)
		if err != nil {
			return err
		}
		if err := o.runCtx.Err(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := o.sink.CommitFact(o.runCtx, o.scope, agent.Fact{Kind: "tool_output", Payload: raw}); err != nil {
			return err
		}
		o.seq++
		start = end
	}
	return nil
}

func (o *callOutput) close() {
	o.mu.Lock()
	o.closed = true
	o.mu.Unlock()
}

type processOutput struct{ output agent.ToolOutputSink }

func (p processOutput) WriteProgress(ctx context.Context, progress agent.ProcessProgress) error {
	if progress.Text == "" {
		return nil // ContentRef and backend Sequence are not public output.
	}
	return p.output.WriteOutput(ctx, agent.ToolOutputChunk{Stream: progress.Stream, Text: progress.Text})
}
