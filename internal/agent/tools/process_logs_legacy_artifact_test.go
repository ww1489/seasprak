package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
)

func TestProcessLogLegacyArtifactReservesFailureEnvelope(t *testing.T) {
	process := &processLogProcess{}
	artifacts := &processLogStore{}
	sink := processLogSink()
	original := agent.ToolObservation{Status: "failed", Process: true, Executed: true, Terminated: true, ExitCode: 23, SideEffect: "confirmed", Truncated: true}
	projection := agent.ToolOutputProjection{CallID: sink.rec.Call.CallID, Truncated: true, Artifact: agent.ArtifactRef{ID: "x", SessionID: "session", Environment: "memory", Hash: "digest", Available: true}}
	projection.Artifact.ID = strings.Repeat("x", 51200-len(projection.ModelContent())+1)
	projection.Observation = original
	if len(projection.ModelContent()) != 51200 {
		t.Fatal("fixture must exactly fit projection envelope")
	}
	sink.rec.Observation, sink.rec.Projection = &original, &projection
	out, err := runProcessLog(t, processLogExecutor(t, process, artifacts, sink))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 51200 || !json.Valid(raw) || out.Artifact.ID != "" || out.LogError != "resource_unavailable: process log reference omitted due to model output limit" {
		t.Errorf("failed model envelope escaped budget: bytes=%d artifactBytes=%d", len(raw), len(out.Artifact.ID))
	}
	if out.ExitCode != 23 || !out.Executed || out.Status != "failed" || *sink.rec.Projection != projection || process.calls != 0 || artifacts.calls != 0 || len(sink.facts) != 0 {
		t.Fatal("failure envelope budgeting changed historical facts or repeated work")
	}
}

func TestProcessLogLegacyOversizedArtifactRecovery(t *testing.T) {
	for _, exit := range []int{0, 23} {
		for _, id := range []string{strings.Repeat("a", 52000), strings.Repeat("<", 9000)} {
			for _, content := range []string{"", "HEAD" + strings.Repeat("中🙂<", 7000) + "TAIL"} {
				t.Run(fmt.Sprintf("exit=%d/id=%d/content=%d", exit, len(id), len(content)), func(t *testing.T) {
					process := &processLogProcess{}
					artifacts := &processLogStore{}
					sink := processLogSink()
					status := "succeeded"
					if exit != 0 {
						status = "failed"
					}
					original := agent.ToolObservation{Status: status, Process: true, Executed: true, Terminated: true, ExitCode: exit, SideEffect: "confirmed", Content: content, Truncated: true}
					projection := agent.ToolOutputProjection{CallID: sink.rec.Call.CallID, Observation: original, Content: content, Artifact: agent.ArtifactRef{ID: id, SessionID: "session", Environment: "memory", Hash: "complete-digest", Size: 60000, Available: true}}
					sink.rec.Observation, sink.rec.Projection = &original, &projection
					before, err := json.Marshal(sink.rec)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(before, &sink.rec); err != nil {
						t.Fatal(err)
					}
					for attempt := 0; attempt < 2; attempt++ {
						out, err := runProcessLog(t, processLogExecutor(t, process, artifacts, sink))
						if err != nil {
							t.Fatal(err)
						}
						model := out.ModelContent()
						if out.Status != "succeeded" {
							raw, err := json.Marshal(out)
							if err != nil {
								t.Fatal(err)
							}
							model = string(raw)
						}
						if len(model) > 51200 || !json.Valid([]byte(model)) {
							t.Errorf("model bytes=%d validJSON=%v", len(model), json.Valid([]byte(model)))
						}
						if out.Artifact.ID != "" || !out.Truncated || out.LogError != "resource_unavailable: process log reference omitted due to model output limit" {
							t.Error("legacy reference was not omitted with truthful model diagnostic")
						}
						if out.Status != status || out.ExitCode != exit || !out.Executed || !out.Terminated || !out.Process || out.SideEffect != "confirmed" {
							t.Error("recovery changed execution facts")
						}
						if content != "" && (!strings.HasPrefix(out.Content, "HEAD") || !strings.HasSuffix(out.Content, "TAIL")) {
							t.Error("reference omission discarded log preview")
						}
						if process.calls != 0 || artifacts.calls != 0 || len(sink.facts) != 0 {
							t.Fatal("recovery repeated execution, saving or fact commits")
						}
						after, err := json.Marshal(sink.rec)
						if err != nil {
							t.Fatal(err)
						}
						if string(after) != string(before) {
							t.Fatal("original persisted reference or observation changed")
						}
					}
				})
			}
		}
	}
}
