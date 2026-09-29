package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProcessModelProjectionBudgetIsPresentationOnly(t *testing.T) {
	text := "HEAD" + strings.Repeat("中🙂\"\\<", 6000) + "TAIL"
	for _, truncated := range []bool{false, true} {
		observation := ToolObservation{Process: true, Status: "failed", ExitCode: 23, Executed: true, Terminated: true, SideEffect: "confirmed", Content: text, Truncated: truncated}
		projection := ToolOutputProjection{Observation: observation, Content: text, Truncated: truncated}
		original := projection
		for name, model := range map[string]string{"observation": observation.ModelContent(), "projection": projection.ModelContent()} {
			if len(model) > 51200 || !json.Valid([]byte(model)) {
				t.Fatalf("%s: bytes=%d validJSON=%v", name, len(model), json.Valid([]byte(model)))
			}
			var envelope struct {
				Content   string
				Truncated bool
			}
			if err := json.Unmarshal([]byte(model), &envelope); err != nil {
				t.Fatal(err)
			}
			if !envelope.Truncated || !utf8.ValidString(envelope.Content) || !strings.HasPrefix(envelope.Content, "HEAD") || !strings.HasSuffix(envelope.Content, "TAIL") {
				t.Fatalf("%s: invalid head/tail preview", name)
			}
		}
		if projection != original || observation != original.Observation {
			t.Fatal("rendering modified source facts")
		}
	}
	short := ToolObservation{Process: true, Content: "short log\n"}
	if short.ModelContent() != short.Content {
		t.Fatal("short process result shape changed")
	}
}

func TestProcessModelBudgetDoesNotTrimFileOrDiscoveryPages(t *testing.T) {
	for _, tail := range []string{`,"nextOffset":60000}`, `,"cursor":"next","matches":3}`} {
		page := `{"content":"` + strings.Repeat("<", 60000) + `"` + tail
		for _, truncated := range []bool{false, true} {
			observation := ToolObservation{Status: "succeeded", Content: page, Truncated: truncated}
			projection := ToolOutputProjection{Observation: observation, Content: page, Truncated: truncated}
			for _, model := range []string{observation.ModelContent(), projection.ModelContent()} {
				if !truncated {
					if model != page {
						t.Fatal("plain page changed")
					}
					continue
				}
				var envelope struct {
					Content   string
					Truncated bool
				}
				if err := json.Unmarshal([]byte(model), &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Content != page || !envelope.Truncated {
					t.Fatal("non-process page or continuation changed")
				}
			}
		}
	}
}
