package llm

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"google.golang.org/genai"
)

const numericRestoreFrame = `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call","name":"lookup","args":{"n":9007199254740993}}}]}}]}`

func numericRestoreSetup(t *testing.T, limit int) (*UsageCollector, *geminiResponseRestorer) {
	t.Helper()
	_, restorer := newGeminiResponseRestorer(t.Context(), limit)
	r := restorer.(*geminiResponseRestorer)
	c := r.collector
	c.gemini.bound = true
	t.Cleanup(r.Close)
	return c, r
}
func numericRestoreSDKResponse(t *testing.T) *genai.GenerateContentResponse {
	t.Helper()
	var resp genai.GenerateContentResponse
	if err := json.Unmarshal([]byte(numericRestoreFrame), &resp); err != nil {
		t.Fatal(err)
	}
	return &resp
}
func numericRestoreInvalid(t *testing.T, err error) {
	t.Helper()
	if e, ok := product.AsError(err); !ok || e.Code != product.CodeInvalidArgument {
		t.Fatalf("unexpected error code: %v", err)
	}
}

func TestGeminiRestoreLineAndPrefetch(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", ""} {
		t.Run(ending, func(t *testing.T) {
			c, r := numericRestoreSetup(t, 4096)
			body := WrapUsageBody(io.NopCloser(strings.NewReader("")), "text/event-stream", c)
			defer body.Close()
			// End before the blank SSE delimiter. SDK already has a full line.
			line := "data: " + numericRestoreFrame + ending
			for i := range line {
				c.consume([]byte{line[i]}, nil)
			}
			if ending == "" {
				c.consume(nil, io.EOF)
			}
			resp := numericRestoreSDKResponse(t)
			if err := r.Restore(resp); err != nil {
				t.Fatal(err)
			}
			if resp.Candidates[0].Content.Parts[0].FunctionCall.Args["n"] != json.Number("9007199254740993") {
				t.Fatal("precision lost")
			}
			if c.gemini.bytes != 0 || len(c.gemini.frames) != 0 {
				t.Fatal("consumed arguments retained")
			}
		})
	}
	c, r := numericRestoreSetup(t, 4096)
	body := WrapUsageBody(io.NopCloser(strings.NewReader(strings.Repeat("data: "+numericRestoreFrame+"\n\n", 3))), "text/event-stream", c)
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if len(c.gemini.frames) != 3 {
		t.Fatal("prefetched frames lost")
	}
	for range 3 {
		if err := r.Restore(numericRestoreSDKResponse(t)); err != nil {
			t.Fatal(err)
		}
	}
	if c.gemini.bytes != 0 {
		t.Fatal("prefetched bytes retained")
	}
}

func TestGeminiRestoreRejectsAssociationMismatch(t *testing.T) {
	for _, mutate := range []struct {
		name  string
		apply func(*genai.GenerateContentResponse)
	}{
		{"id", func(r *genai.GenerateContentResponse) { r.Candidates[0].Content.Parts[0].FunctionCall.ID = "other" }},
		{"name", func(r *genai.GenerateContentResponse) { r.Candidates[0].Content.Parts[0].FunctionCall.Name = "other" }},
		{"candidate", func(r *genai.GenerateContentResponse) { r.Candidates[0].Index = 1 }},
		{"position", func(r *genai.GenerateContentResponse) {
			r.Candidates[0].Content.Parts = append([]*genai.Part{{Text: "other"}}, r.Candidates[0].Content.Parts...)
		}},
		{"missing", func(r *genai.GenerateContentResponse) { r.Candidates[0].Content.Parts[0].FunctionCall = nil }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			c, r := numericRestoreSetup(t, 4096)
			c.collectGeminiArguments([]byte(numericRestoreFrame))
			resp := numericRestoreSDKResponse(t)
			mutate.apply(resp)
			numericRestoreInvalid(t, r.Restore(resp))
		})
	}
}

func TestGeminiRestoreCumulativeLimitAndClose(t *testing.T) {
	c, r := numericRestoreSetup(t, 512)
	c.collectGeminiArguments([]byte(numericRestoreFrame))
	if c.gemini.invalid {
		t.Fatal("single bounded frame rejected")
	}
	c.collectGeminiArguments([]byte(numericRestoreFrame))
	c.collectGeminiArguments([]byte(numericRestoreFrame))
	numericRestoreInvalid(t, r.Restore(numericRestoreSDKResponse(t)))
	if c.gemini.bytes > 512 {
		t.Fatal("retained budget exceeded")
	}
	r.Close()
	r.Close()
	if c.gemini.bytes != 0 || c.gemini.frames != nil || c.buffer != nil {
		t.Fatal("close retained arguments")
	}
	c.collectGeminiArguments([]byte(numericRestoreFrame))
	if c.gemini.frames != nil {
		t.Fatal("capture resumed after close")
	}
	numericRestoreInvalid(t, r.Restore(numericRestoreSDKResponse(t)))
}

func TestGeminiRestoreUsageDegradationAndReuse(t *testing.T) {
	c, r := numericRestoreSetup(t, 512)
	body := WrapUsageBody(io.NopCloser(strings.NewReader("")), "text/event-stream", c)
	defer body.Close()
	// Invalid measurements must disable only usage, not exact argument capture.
	badUsage := strings.TrimSuffix(numericRestoreFrame, "}") + `,"usageMetadata":{"promptTokenCount":-1}}`
	for i := range 20 {
		frame := numericRestoreFrame
		if i == 0 {
			frame = badUsage
		}
		c.consume([]byte("data: "+frame+"\n"), nil)
		if err := r.Restore(numericRestoreSDKResponse(t)); err != nil {
			t.Fatal(err)
		}
		if c.gemini.bytes != 0 {
			t.Fatal("consumed frame retained budget")
		}
	}
	if c.Snapshot().Diagnostic != "invalid_usage_measurement" {
		t.Fatal("usage degradation changed")
	}
}

func TestGeminiRestoreCloseAndReadConcurrent(t *testing.T) {
	for range 20 {
		c, r := numericRestoreSetup(t, 4096)
		body := WrapUsageBody(io.NopCloser(strings.NewReader("")), "text/event-stream", c)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			for range 20 {
				c.consume([]byte("data: "+numericRestoreFrame+"\n"), nil)
			}
		}()
		go func() { defer wg.Done(); r.Close() }()
		go func() { defer wg.Done(); _ = body.Close() }()
		wg.Wait()
		if c.gemini.frames != nil || c.gemini.bytes != 0 || c.buffer != nil {
			t.Fatal("concurrent close retained capture")
		}
	}
}

func TestGeminiRestoreCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	_, restorer := newGeminiResponseRestorer(ctx, 4096)
	r := restorer.(*geminiResponseRestorer)
	r.collector.gemini.bound = true
	r.collector.collectGeminiArguments([]byte(numericRestoreFrame))
	cancel()
	if err := r.Restore(numericRestoreSDKResponse(t)); err != context.Canceled {
		t.Fatal("cancellation not preserved")
	}
	r.Close()
	r.collector.mu.Lock()
	defer r.collector.mu.Unlock()
	if r.collector.gemini.bytes != 0 || r.collector.gemini.frames != nil {
		t.Fatal("cancelled capture retained data")
	}
}
