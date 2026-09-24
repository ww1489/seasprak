package llm_test

import (
	"context"
	"io"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestModelContractGenerateAndStream(t *testing.T) {
	fake := testkit.NewFake(testkit.Step{Text: "pong", Repeat: true})
	var model llm.Model = fake
	msg, err := model.Generate(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := textOf(msg); got != "pong" {
		t.Fatalf("generate text = %q", got)
	}
	reader, err := model.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	streamed, err := reader.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Recv(); err != io.EOF {
		t.Fatalf("stream end = %v", err)
	}
	if got := textOf(streamed); got != "pong" {
		t.Fatalf("stream text = %q", got)
	}
	if fake.Calls() != 2 {
		t.Fatalf("calls = %d", fake.Calls())
	}
}

func TestModelContractCanceledGenerate(t *testing.T) {
	gate := make(chan struct{})
	fake := testkit.NewFake(testkit.Step{Text: "late", Gate: gate})
	var model llm.Model = fake
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := model.Generate(ctx, nil)
	if err != context.Canceled {
		t.Fatalf("error = %v", err)
	}
	if fake.Calls() != 1 {
		t.Fatalf("calls = %d", fake.Calls())
	}
}

func textOf(msg *schema.AgenticMessage) string {
	if msg == nil {
		return ""
	}
	for _, block := range msg.ContentBlocks {
		if block == nil || block.AssistantGenText == nil {
			continue
		}
		if block.AssistantGenText != nil {
			return block.AssistantGenText.Text
		}
	}
	return ""
}
