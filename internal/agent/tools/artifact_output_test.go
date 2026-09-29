package tools

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

func TestProcessOutputIsSavedAsArtifactWithoutRerunning(t *testing.T) {
	process := &artifactProcessProbe{}
	artifacts := fixture.NewMemory()
	def := Definition{Name: "execute", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: ExecutionDescription{BackendID: "process-operations", Effect: "unknown", Argv: []string{"runner"}}}
	sink := &recordSink{found: true, rec: builtinAccepted(`{}`, "execute", "provider-output")}
	sink.afterCommit = func(f agent.Fact) {
		switch f.Kind {
		case "tool_observation":
			if err := json.Unmarshal(f.Payload, &sink.rec); err != nil {
				t.Fatal(err)
			}
		case "tool_output_projection":
			var p agent.ToolOutputProjection
			if err := json.Unmarshal(f.Payload, &p); err != nil {
				t.Fatal(err)
			}
			sink.rec.Projection = &p
		}
	}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Process: process, Artifacts: artifacts, OutputRedactor: func(_ context.Context, text string) (string, error) { return text, nil }}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "provider-output", "execute", `{}`)
	if err != nil || out.Status != "succeeded" || out.Artifact.ID == "" || out.LogError != "" || process.calls.Load() != 1 || artifacts.Calls("save_output") != 1 || artifacts.Calls("save") != 0 {
		t.Fatalf("status=%s reference=%s logError=%q starts=%d saves=%d err=%v", out.Status, out.Artifact.ID, out.LogError, process.calls.Load(), artifacts.Calls("save_output"), err)
	}
	repeated, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "provider-output", "execute", `{}`)
	if err != nil || repeated != out || process.calls.Load() != 1 || artifacts.Calls("save_output") != 1 {
		t.Fatal("observed call repeated execution or log save", err)
	}
	reader, err := artifacts.OpenOutput(t.Context(), agent.OutputArtifactRead{Binding: agent.OutputArtifactBinding{SessionID: "session", Environment: "memory", CallID: sink.rec.Call.CallID}, Ref: out.Artifact})
	if err != nil {
		t.Fatal(err)
	}
	full, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(full) != strings.Repeat("完整日志\n", 2100) {
		t.Fatal("full output was not readable", err)
	}
}

type artifactProcessProbe struct{ calls atomic.Int32 }

func (p *artifactProcessProbe) Execute(ctx context.Context, req agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.calls.Add(1)
	return agent.ProcessObservation{Started: true, Terminated: true, Content: strings.Repeat("完整日志\n", 2100), SideEffect: "none"}, nil
}
func (*artifactProcessProbe) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}
