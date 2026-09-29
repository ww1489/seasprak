package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
)

func TestProcessLogJSONBudgetForShortRawFailure(t *testing.T) {
	// A raw log below 50 KiB can still need an artifact once the failed
	// execution envelope escapes '<'. The saved text must be complete.
	for _, failSave := range []bool{false, true} {
		text := "HEAD" + strings.Repeat("<", 49000) + "TAIL"
		p := &processLogProcess{observation: processLogObservation(text)}
		p.observation.ExitCode = 23
		s := &processLogStore{}
		if failSave {
			s.save = func(context.Context, agent.OutputArtifactInput) (agent.ArtifactRef, error) {
				return agent.ArtifactRef{}, errors.New("synthetic save failure")
			}
		}
		sink := processLogSink()
		out, err := runProcessLog(t, processLogExecutor(t, p, s, sink))
		if err != nil {
			t.Fatal(err)
		}
		assertProcessOutcomeBudget(t, out)
		if out.Status != "failed" || out.ExitCode != 23 || !out.Terminated || !out.Executed || out.SideEffect != "confirmed" || s.input.Content != text || p.calls != 1 || s.calls != 1 {
			t.Fatalf("failed outcome or full-log save changed: status=%s exit=%d process=%d saves=%d", out.Status, out.ExitCode, p.calls, s.calls)
		}
		if failSave && (out.Artifact.ID != "" || !strings.HasPrefix(out.LogError, "resource_unavailable:")) {
			t.Fatal("save failure lost error code")
		}
		if !failSave && out.Artifact != processLogRef(s.input) {
			t.Fatal("saved artifact identity changed")
		}
		obs := lastObservation(t, sink)
		if len(obs.ModelContent()) > 51200 || !json.Valid([]byte(obs.ModelContent())) {
			t.Fatal("observation fallback exceeds serialized budget")
		}
	}
}

func TestProcessLogJSONBudgetPreservesProjectionFailureFallback(t *testing.T) {
	p := &processLogProcess{observation: processLogObservation("HEAD" + strings.Repeat(`"`, 60000) + "TAIL")}
	s := &processLogStore{}
	sink := processLogSink()
	sink.beforeCommit = func(f agent.Fact) error {
		if f.Kind == "tool_output_projection" {
			return errors.New("synthetic commit failure")
		}
		return nil
	}
	sink.afterCommit = func(f agent.Fact) {
		if f.Kind == "tool_observation" {
			if err := json.Unmarshal(f.Payload, &sink.rec); err != nil {
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
	restored, err := runProcessLog(t, exec)
	if err != nil || restored != out || out.Artifact.ID != "" || out.LogError != logNotSaved || p.calls != 1 || s.calls != 1 {
		t.Fatalf("projection fallback replay changed: process=%d saves=%d err=%v", p.calls, s.calls, err)
	}
}

func TestProcessLogJSONBudgetRetainsEscapedArtifactIdentity(t *testing.T) {
	p := &processLogProcess{observation: processLogObservation("HEAD" + strings.Repeat(`"`, 60000) + "TAIL")}
	p.observation.ExitCode = 23
	s := &processLogStore{save: func(_ context.Context, in agent.OutputArtifactInput) (agent.ArtifactRef, error) {
		ref := processLogRef(in)
		ref.ID = "artifact:" + strings.Repeat("<", 1500)
		return ref, nil
	}}
	sink := processLogSink()
	out, err := runProcessLog(t, processLogExecutor(t, p, s, sink))
	if err != nil {
		t.Fatal(err)
	}
	assertProcessOutcomeBudget(t, out)
	want := processLogRef(s.input)
	want.ID = "artifact:" + strings.Repeat("<", 1500)
	if out.Artifact != want || out.ExitCode != 23 || p.calls != 1 || s.calls != 1 {
		t.Fatal("escaped artifact identity or execution facts changed")
	}
	var projected agent.ToolOutputProjection
	for _, f := range sink.facts {
		if f.Kind == "tool_output_projection" {
			if err := json.Unmarshal(f.Payload, &projected); err != nil {
				t.Fatal(err)
			}
		}
	}
	if projected.Artifact != want || len(projected.ModelContent()) > 51200 || !json.Valid([]byte(projected.ModelContent())) {
		t.Fatal("persisted artifact projection exceeds budget")
	}
}

func TestProcessLogJSONBudgetOnLegacyObservationAndProjection(t *testing.T) {
	for _, projected := range []bool{false, true} {
		p := &processLogProcess{}
		s := &processLogStore{}
		sink := processLogSink()
		original := agent.ToolObservation{Status: "failed", Content: "HEAD" + strings.Repeat("<", 51000) + "TAIL", Process: true, Executed: true, Terminated: true, ExitCode: 23, SideEffect: "confirmed", Truncated: true, LogError: logNotSaved}
		sink.rec.Observation = &original
		var ref agent.ArtifactRef
		if projected {
			ref = processLogRef(agent.OutputArtifactInput{Content: "original full log", Binding: agent.OutputArtifactBinding{SessionID: "session", Environment: "memory"}})
			sink.rec.Projection = &agent.ToolOutputProjection{CallID: sink.rec.Call.CallID, Observation: original, Content: original.Content, Truncated: true, Artifact: ref}
		}
		out, err := runProcessLog(t, processLogExecutor(t, p, s, sink))
		if err != nil {
			t.Fatal(err)
		}
		assertProcessOutcomeBudget(t, out)
		if out.Status != original.Status || out.ExitCode != original.ExitCode || out.Artifact != ref || out.SideEffect != original.SideEffect || !out.Terminated || !out.Executed || p.calls != 0 || s.calls != 0 || *sink.rec.Observation != original {
			t.Fatal("legacy presentation changed evidence or replayed execution")
		}
	}
}

func TestProcessLogBudgetLeavesFileContinuationUntouched(t *testing.T) {
	content := `{"content":"` + strings.Repeat("<", 60000) + `","nextOffset":60000}`
	obs := agent.ToolObservation{Status: "succeeded", Content: content, Truncated: true}
	out := outcomeOfRecord(agent.ToolRecord{Observation: &obs})
	if out.Content != content || !json.Valid([]byte(out.Content)) {
		t.Fatal("process budget changed non-process continuation")
	}
}

func assertProcessOutcomeBudget(t *testing.T, out Outcome) {
	t.Helper()
	model := out.ModelContent()
	if out.Status != "succeeded" {
		body, _ := json.Marshal(out)
		model = string(body)
	}
	if len(model) > 51200 || !json.Valid([]byte(model)) || !utf8.ValidString(out.Content) || !out.Truncated || !strings.HasPrefix(out.Content, "HEAD") || !strings.HasSuffix(out.Content, "TAIL") {
		t.Errorf("invalid process preview: modelBytes=%d validJSON=%v truncated=%v", len(model), json.Valid([]byte(model)), out.Truncated)
	}
}
