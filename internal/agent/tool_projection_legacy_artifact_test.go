package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProcessModelOversizedArtifactLeavesOriginalAndNonProcessUntouched(t *testing.T) {
	for _, process := range []bool{false, true} {
		p := ToolOutputProjection{Observation: ToolObservation{Process: process, Status: "failed", Executed: true, Terminated: true, ExitCode: 23}, Content: "safe preview", Artifact: ArtifactRef{ID: strings.Repeat("<", 9000), SessionID: "session", Environment: "memory", Hash: "digest", Size: 60000, Available: true}}
		original := p
		copy := p.ForModel(51200)
		model := p.ModelContent()
		if p != original || copy.Observation != original.Observation {
			t.Fatal("presentation changed original facts")
		}
		var body struct {
			Content   string
			Truncated bool
			Artifact  *ArtifactRef
			LogError  string
		}
		if err := json.Unmarshal([]byte(model), &body); err != nil {
			t.Fatal(err)
		}
		if process {
			if len(model) > 51200 || body.Artifact != nil || !body.Truncated || body.Content != original.Content || body.LogError != "resource_unavailable: process log reference omitted due to model output limit" {
				t.Fatal("invalid process-only reference omission")
			}
		} else if copy != original || body.Artifact == nil || *body.Artifact != original.Artifact || body.Truncated || body.LogError != "" {
			t.Fatal("non-process reference changed")
		}
	}
}

func TestProcessModelArtifactAtBudgetRemainsVisible(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		p := ToolOutputProjection{Artifact: ArtifactRef{ID: "x"}, Truncated: truncated}
		p.Artifact.ID = strings.Repeat("x", 51200-len(p.ModelContent())+1)
		p.Observation.Process = true
		original := p
		copy := p.ForModel(51200)
		if copy != original || len(copy.ModelContent()) != 51200 {
			t.Fatal("a fitting reference was unnecessarily omitted")
		}
	}
}
