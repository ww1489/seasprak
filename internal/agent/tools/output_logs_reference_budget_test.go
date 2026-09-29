package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
)

func oversizedReferenceStore(id string) *processLogStore {
	return &processLogStore{save: func(_ context.Context, in agent.OutputArtifactInput) (agent.ArtifactRef, error) {
		ref := processLogRef(in)
		ref.ID = id
		return ref, nil
	}}
}

func TestSaveOutputLogRejectsUnpublishableReference(t *testing.T) {
	for _, id := range []string{strings.Repeat("a", 52000), strings.Repeat("<", 9000), strings.Repeat(`"`, 26000), strings.Repeat("a", 51000)} {
		t.Run(fmt.Sprintf("bytes=%d/first=%q", len(id), id[:1]), func(t *testing.T) {
			s := oversizedReferenceStore(id)
			in := agent.OutputArtifactInput{Binding: agent.OutputArtifactBinding{SessionID: "session", Environment: "memory", CallID: "call"}, Content: "complete redacted log"}
			ref, logError := SaveOutputLog(t.Context(), s, in)
			if ref != (agent.ArtifactRef{}) || logError != "resource_unavailable: saved process log reference exceeds model output limit" {
				t.Errorf("unpublishable ref escaped: refBytes=%d logError=%q", len(ref.ID), logError)
			}
			if s.calls != 1 || s.input != in || s.legacyCalls != 0 {
				t.Fatal("save invocation or original full output changed")
			}
		})
	}
}

func TestProcessLogLargePublishableReferenceRetainsIdentity(t *testing.T) {
	p := &processLogProcess{observation: processLogObservation("HEAD" + strings.Repeat("<", 60000) + "TAIL")}
	p.observation.ExitCode = 23
	id := strings.Repeat("a", 49000)
	s := oversizedReferenceStore(id)
	out, err := runProcessLog(t, processLogExecutor(t, p, s, processLogSink()))
	if err != nil {
		t.Fatal(err)
	}
	assertProcessOutcomeBudget(t, out)
	want := processLogRef(s.input)
	want.ID = id
	if out.Artifact != want || out.LogError != "" || p.calls != 1 || s.calls != 1 || out.ExitCode != 23 {
		t.Fatal("publishable reference identity or execution facts changed")
	}
}

func TestProcessLogUnpublishableReferencePreservesExecution(t *testing.T) {
	for _, exit := range []int{0, 23} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			p := &processLogProcess{observation: processLogObservation("HEAD" + strings.Repeat("<", 60000) + "TAIL")}
			p.observation.ExitCode = exit
			s := oversizedReferenceStore(strings.Repeat("<", 9000))
			sink := processLogSink()
			sink.afterCommit = func(f agent.Fact) {
				switch f.Kind {
				case "tool_observation":
					if err := json.Unmarshal(f.Payload, &sink.rec); err != nil {
						t.Fatal(err)
					}
				case "tool_output_projection":
					if err := json.Unmarshal(f.Payload, &sink.rec.Projection); err != nil {
						t.Fatal(err)
					}
				}
			}
			exec := processLogExecutor(t, p, s, sink)
			out, err := runProcessLog(t, exec)
			if err != nil {
				t.Fatal(err)
			}
			assertProcessOutcomeBudget(t, out)
			wantStatus := "succeeded"
			if exit != 0 {
				wantStatus = "failed"
			}
			if out.Status != wantStatus || out.ExitCode != exit || !out.Executed || !out.Terminated || out.SideEffect != "confirmed" || out.Artifact.ID != "" || out.LogError != "resource_unavailable: saved process log reference exceeds model output limit" {
				t.Errorf("execution facts or reference rejection incorrect: status=%s exit=%d refBytes=%d logError=%q", out.Status, out.ExitCode, len(out.Artifact.ID), out.LogError)
			}
			if p.calls != 1 || s.calls != 1 || s.input.Content != p.observation.Content {
				t.Fatal("execution/save count or full output changed")
			}
			restored, err := runProcessLog(t, processLogExecutor(t, p, s, sink))
			if err != nil || restored != out || p.calls != 1 || s.calls != 1 {
				t.Fatal("recovery repeated side effects or changed result", err)
			}
			if sink.rec.Projection == nil || sink.rec.Projection.Artifact.ID != "" {
				t.Error("oversized artifact was published to durable projection")
			}
		})
	}
}
