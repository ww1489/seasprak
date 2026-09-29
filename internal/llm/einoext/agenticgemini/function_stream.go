package agenticgemini

import (
	"encoding/json"
	"strings"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"google.golang.org/genai"
)

// Some compatible gateways emit a named empty call followed by anonymous
// args:{"arguments":"<JSON delta>"} parts. This is not the native PartialArgs
// protocol. Recognize only this unambiguous sequential shape; never merge two
// named calls, infer a missing name, or reinterpret a named business argument.
// One instance belongs to one stream and retains at most one bounded call.
type functionCallStream struct {
	pending    *genai.Part
	fragments  strings.Builder
	fragmented bool
}

func invalidFunctionStream() error {
	return product.NewError(product.CodeInvalidArgument, "ambiguous or incomplete Gemini function argument stream")
}

func (s *functionCallStream) flush(parts *[]*genai.Part) error {
	if s.pending == nil {
		return nil
	}
	if s.fragmented {
		var args map[string]any
		decoder := json.NewDecoder(strings.NewReader(s.fragments.String()))
		decoder.UseNumber()
		// json.Valid also rejects trailing values without losing integer precision.
		if !json.Valid([]byte(s.fragments.String())) || decoder.Decode(&args) != nil || args == nil {
			return invalidFunctionStream()
		}
		s.pending.FunctionCall.Args = args
	}
	*parts = append(*parts, s.pending)
	s.pending = nil
	s.fragments.Reset()
	s.fragmented = false
	return nil
}

func (s *functionCallStream) consume(resp *genai.GenerateContentResponse) error {
	if resp == nil || len(resp.Candidates) == 0 {
		return nil
	}
	if len(resp.Candidates) != 1 || resp.Candidates[0] == nil {
		return invalidFunctionStream()
	}
	candidate := resp.Candidates[0]
	var parts []*genai.Part
	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			if part == nil {
				return invalidFunctionStream()
			}
			call := part.FunctionCall
			if call != nil && call.Name == "" {
				// A signature-only part has no function-call payload and does not enter here.
				if s.pending == nil || len(call.Args) != 1 || len(call.PartialArgs) != 0 || call.WillContinue != nil || len(part.ThoughtSignature) != 0 || call.ID != "" && call.ID != s.pending.FunctionCall.ID {
					return invalidFunctionStream()
				}
				delta, ok := call.Args["arguments"].(string)
				if !ok || s.fragments.Len()+len(delta) > config.GeminiFunctionArgumentsBytes {
					return invalidFunctionStream()
				}
				s.fragmented = true
				s.fragments.WriteString(delta)
				continue
			}
			if err := s.flush(&parts); err != nil {
				return err
			}
			if call != nil && call.Name != "" && len(call.Args) == 0 && len(call.PartialArgs) == 0 && call.WillContinue == nil {
				s.pending = part
			} else {
				parts = append(parts, part)
			}
		}
	}
	if candidate.FinishReason != "" {
		if err := s.flush(&parts); err != nil {
			return err
		}
	}
	if candidate.Content == nil && len(parts) > 0 {
		candidate.Content = &genai.Content{Role: roleModel}
	}
	if candidate.Content != nil {
		candidate.Content.Parts = parts
	}
	return nil
}
