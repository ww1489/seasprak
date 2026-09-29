package agenticgemini

import (
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"google.golang.org/genai"
)

func functionFrame(parts ...*genai.Part) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{Role: roleModel, Parts: parts}}}}
}
func namedFunction(name string, args map[string]any) *genai.Part {
	return &genai.Part{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}
}
func argumentDelta(value string) *genai.Part {
	return namedFunction("", map[string]any{"arguments": value})
}

func TestFunctionStreamRejectsAmbiguousFragments(t *testing.T) {
	for _, kind := range []string{"orphan", "wrong_id", "wrong_key", "non_string", "oversize", "truncated", "array", "null", "trailing"} {
		t.Run(kind, func(t *testing.T) {
			var s functionCallStream
			if kind != "orphan" {
				if err := s.consume(functionFrame(namedFunction("lookup", map[string]any{}))); err != nil {
					t.Fatal(err)
				}
			}
			part := argumentDelta(`{"q":"ping"}`)
			switch kind {
			case "wrong_id":
				part.FunctionCall.ID = "other"
			case "wrong_key":
				part.FunctionCall.Args = map[string]any{"q": "ping"}
			case "non_string":
				part.FunctionCall.Args["arguments"] = 1
			case "oversize":
				part = argumentDelta(strings.Repeat("x", config.GeminiFunctionArgumentsBytes+1))
			case "truncated":
				part = argumentDelta(`{"q":`)
			case "array":
				part = argumentDelta(`[]`)
			case "null":
				part = argumentDelta(`null`)
			case "trailing":
				part = argumentDelta(`{} {}`)
			}
			resp := functionFrame(part)
			resp.Candidates[0].FinishReason = genai.FinishReasonStop
			err := s.consume(resp)
			pe, ok := product.AsError(err)
			if !ok || pe.Code != product.CodeInvalidArgument {
				t.Fatalf("expected invalid_argument, got %v", err)
			}
		})
	}
}

func TestFunctionStreamPreservesCallsAndSignatures(t *testing.T) {
	var s functionCallStream
	first := namedFunction("lookup", map[string]any{})
	first.ThoughtSignature = []byte("synthetic")
	if err := s.consume(functionFrame(first)); err != nil {
		t.Fatal(err)
	}
	if err := s.consume(functionFrame(argumentDelta(`{"n":9007199254740993}`))); err != nil {
		t.Fatal(err)
	}
	business := namedFunction("lookup", map[string]any{"arguments": "business"})
	resp := functionFrame(business)
	if err := s.consume(resp); err != nil {
		t.Fatal(err)
	}
	parts := resp.Candidates[0].Content.Parts
	if len(parts) != 2 || string(parts[0].ThoughtSignature) != "synthetic" || parts[1] != business {
		t.Fatal("calls or signature lost")
	}
	a, err := convAgenticFC(parts[0].FunctionCall)
	if err != nil {
		t.Fatal(err)
	}
	b, err := convAgenticFC(parts[1].FunctionCall)
	if err != nil {
		t.Fatal(err)
	}
	if a.FunctionToolCall.CallID == b.FunctionToolCall.CallID || a.FunctionToolCall.Arguments != `{"n":9007199254740993}` || b.FunctionToolCall.Arguments != `{"arguments":"business"}` {
		t.Fatal("call identities or exact business arguments changed")
	}
}
