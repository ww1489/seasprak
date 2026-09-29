package llm_test

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// Only counts and fixed classifications leave this diagnostic. Never retain
// provider text, argument keys/values, signatures, endpoints or credentials.
type liveGeminiShape struct {
	Frames, Candidates, Calls, MissingCallIDs, TextParts int
	NamedCalls, ObjectArgs, EmptyArgs, ArgumentFragments int
	Stop, Length, OtherFinish, RequiredFieldError        bool
	InvalidAPIKey, LocationUnsupported                   bool
}

func liveGeminiResponseShape(raw []byte) liveGeminiShape {
	var shape liveGeminiShape
	inspect := func(frame []byte) {
		var body struct {
			Error struct {
				Message string `json:"message"`
				Details []struct {
					Reason string `json:"reason"`
				} `json:"details"`
			} `json:"error"`
			Candidates []struct {
				FinishReason string `json:"finishReason"`
				Content      struct {
					Parts []struct {
						Text string `json:"text"`
						Call *struct {
							ID   string          `json:"id"`
							Name string          `json:"name"`
							Args json.RawMessage `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if json.Unmarshal(frame, &body) != nil {
			return
		}
		shape.Frames++
		for _, detail := range body.Error.Details {
			shape.InvalidAPIKey = shape.InvalidAPIKey || detail.Reason == "API_KEY_INVALID"
		}
		shape.LocationUnsupported = shape.LocationUnsupported || strings.Contains(strings.ToLower(body.Error.Message), "user location is not supported")
		shape.RequiredFieldError = shape.RequiredFieldError || strings.Contains(strings.ToLower(body.Error.Message), "required field")
		for _, candidate := range body.Candidates {
			shape.Candidates++
			switch candidate.FinishReason {
			case "STOP":
				shape.Stop = true
			case "MAX_TOKENS":
				shape.Length = true
			case "":
			default:
				shape.OtherFinish = true
			}
			for _, part := range candidate.Content.Parts {
				if part.Text != "" {
					shape.TextParts++
				}
				if part.Call == nil {
					continue
				}
				shape.Calls++
				if part.Call.ID == "" {
					shape.MissingCallIDs++
				}
				if part.Call.Name != "" {
					shape.NamedCalls++
				}
				var args map[string]any
				if json.Unmarshal(part.Call.Args, &args) == nil && args != nil {
					shape.ObjectArgs++
					if len(args) == 0 {
						shape.EmptyArgs++
					}
					if _, ok := args["arguments"].(string); ok && len(args) == 1 && part.Call.Name == "" {
						shape.ArgumentFragments++
					}
				}
			}
		}
	}
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		inspect(raw)
	} else {
		for _, line := range bytes.Split(raw, []byte("\n")) {
			if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				inspect(bytes.TrimSpace(data))
			}
		}
	}
	return shape
}

type liveGeminiDiagnosticBody struct {
	io.ReadCloser
	t         *testing.T
	status    int
	collected []byte
	overflow  bool
}

func (b *liveGeminiDiagnosticBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.overflow {
		if len(b.collected)+n > 1<<20 {
			b.overflow = true
			b.collected = nil
		} else {
			b.collected = append(b.collected, p[:n]...)
		}
	}
	return n, err
}
func (b *liveGeminiDiagnosticBody) Close() error {
	err := b.ReadCloser.Close()
	if !b.overflow {
		b.t.Logf("gemini_http_status=%d shape=%+v", b.status, liveGeminiResponseShape(b.collected))
	}
	b.collected = nil
	return err
}
func TestLiveGeminiErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		body                 string
		invalidKey, location bool
	}{
		{`{"error":{"message":"synthetic-private","details":[{"reason":"API_KEY_INVALID"}]}}`, true, false},
		{`{"error":{"message":"User location is not supported for the API use."}}`, false, true},
		{`{"error":{"message":"synthetic-private","details":[{"reason":"synthetic-private"}]}}`, false, false},
	} {
		s := liveGeminiResponseShape([]byte(tc.body))
		if s.InvalidAPIKey != tc.invalidKey || s.LocationUnsupported != tc.location {
			t.Fatal("incorrect fixed error classification")
		}
		raw, _ := json.Marshal(s)
		if strings.Contains(string(raw), "synthetic-private") {
			t.Fatal("private error content leaked")
		}
	}
}

func TestLiveGeminiShapeReportsOnlyStructure(t *testing.T) {
	raw := `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"synthetic-private","args":{"synthetic-private":"synthetic-private"}}},{"text":"synthetic-private"}]}}]}`
	for _, body := range []string{raw, "data: " + raw + "\n\n"} {
		s := liveGeminiResponseShape([]byte(body))
		if s.Calls != 1 || s.MissingCallIDs != 1 || s.TextParts != 1 || !s.Stop {
			t.Fatal("structural diagnostics mismatch")
		}
		encoded, _ := json.Marshal(s)
		if strings.Contains(string(encoded), "synthetic-private") {
			t.Fatal("diagnostics retained private content")
		}
	}
}
