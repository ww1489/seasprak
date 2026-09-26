package sessions

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPauseCapturesResumeSourceAndTrustedCompatibility(t *testing.T) {
	f := pausedResumeFixture(t, false)
	for _, cp := range f.manager.View().Checkpoints {
		if !strings.HasPrefix(cp.BuildCompatibility, "resume-v1:") || cp.ManifestHash == "" {
			t.Fatalf("pause lacks explicit implementation compatibility: build=%q manifest=%q", cp.BuildCompatibility, cp.ManifestHash)
		}
		raw, err := json.Marshal(cp)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Input struct{ InputID, TraceID, Kind string }
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Input.InputID != f.input.InputID || envelope.Input.TraceID != f.input.TraceID || envelope.Input.Kind != "prompt" {
			t.Fatalf("pause lost original resume input: %+v", envelope.Input)
		}
	}
}
