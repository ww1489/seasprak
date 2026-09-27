package tools

import (
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestProcessOutputIsSavedAsArtifactWithoutRerunning(t *testing.T) {
	process := &artifactProcessProbe{}
	artifacts := &artifactSaveProbe{}
	def := Definition{Name: "execute", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: ExecutionDescription{BackendID: "process-operations", Effect: "unknown", Argv: []string{"runner"}}}
	sink := &recordSink{found: true, rec: builtinAccepted(`{}`, "execute", "provider-output")}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Process: process, Artifacts: artifacts}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "provider-output", "execute", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "succeeded" || out.Content != "artifact:full" || process.calls.Load() != 1 || artifacts.saves.Load() != 1 {
		t.Fatalf("output was not saved exactly once: out=%+v process=%d artifacts=%d", out, process.calls.Load(), artifacts.saves.Load())
	}
}

type artifactProcessProbe struct{ calls atomic.Int32 }

func (p *artifactProcessProbe) Execute(ctx context.Context, req agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.calls.Add(1)
	return agent.ProcessObservation{Started: true, Terminated: true, ContentRef: "private:log", SideEffect: "none"}, nil
}
func (*artifactProcessProbe) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}

type artifactSaveProbe struct{ saves atomic.Int32 }

func (a *artifactSaveProbe) Save(_ context.Context, _ agent.ArtifactInput) (agent.ArtifactRef, error) {
	a.saves.Add(1)
	return agent.ArtifactRef{ID: "artifact:full", Available: true}, nil
}
func (*artifactSaveProbe) Open(context.Context, agent.ArtifactRead) (io.ReadCloser, error) {
	return nil, product.NewError(product.CodeNotFound, "not used")
}
