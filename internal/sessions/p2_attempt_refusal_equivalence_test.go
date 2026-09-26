package sessions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/config"
)

type p2RefusalSink struct{ facts []agent.Fact }

func (s *p2RefusalSink) CommitFact(_ context.Context, _ agent.ExecutionScope, f agent.Fact) error {
	s.facts = append(s.facts, f)
	return nil
}

func TestP2AttemptRefusalGenerateStreamDiagnosticEquivalent(t *testing.T) {
	var results []agent.ModelAttemptDetails
	for _, stream := range []bool{false, true} {
		var calls atomic.Int32
		model := p2FactoryModel(t, func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			body := `{"choices":[{"index":0,"message":{"role":"assistant","refusal":"无法协助😀"},"finish_reason":"content_filter"}]}`
			contentType := "application/json"
			if stream {
				body = "data: " + `{"choices":[{"index":0,"delta":{"role":"assistant","refusal":"无法"},"finish_reason":null}]}` + "\n\ndata: " + `{"choices":[{"index":0,"delta":{"refusal":"协助😀"},"finish_reason":"content_filter"}]}` + "\n\ndata: [DONE]\n\n"
				contentType = "text/event-stream"
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})
		sink := &p2RefusalSink{}
		vm := einorun.NewValidatedModel(model, sink, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{})
		var err error
		if stream {
			_, err = vm.Stream(t.Context(), nil)
		} else {
			_, err = vm.Generate(t.Context(), nil)
		}
		if err == nil || calls.Load() != 1 {
			t.Fatal("refusal accepted or request count incorrect")
		}
		terminals := 0
		for _, f := range sink.facts {
			if f.Kind == "assistant" {
				terminals++
				var body struct {
					Status  string                    `json:"status"`
					Details agent.ModelAttemptDetails `json:"details"`
				}
				if err = json.Unmarshal(f.Payload, &body); err != nil {
					t.Fatal(err)
				}
				if body.Status != "incomplete" || body.Details.RefusalReason != "无法协助😀" || body.Details.OriginalFinishReason != "content_filter" || body.Details.FailureReason != "refusal" {
					t.Fatal("product refusal diagnostic missing")
				}
				// Usage identities differ between attempts and are tested independently.
				body.Details.Usage = nil
				results = append(results, body.Details)
			}
		}
		if terminals != 1 {
			t.Fatal("refusal did not produce exactly one terminal")
		}
	}
	a, _ := json.Marshal(results[0])
	b, _ := json.Marshal(results[1])
	if string(a) != string(b) {
		t.Fatal("Generate/Stream failure diagnostics differ")
	}
}
