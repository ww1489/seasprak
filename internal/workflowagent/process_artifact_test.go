package workflowagent

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

type workflowOutputArtifacts struct {
	*fixture.Memory
	input agent.OutputArtifactInput
	saves int
}

func (a *workflowOutputArtifacts) SaveOutput(ctx context.Context, in agent.OutputArtifactInput) (agent.ArtifactRef, error) {
	if err := ctx.Err(); err != nil {
		return agent.ArtifactRef{}, err
	}
	a.saves++
	a.input = in
	return agent.ArtifactRef{ID: "output", WorkflowRunID: in.Binding.WorkflowRunID, SessionID: in.Binding.SessionID, Environment: in.Binding.Environment, Hash: rawHash([]byte(in.Content)), Size: int64(len(in.Content)), Available: true}, nil
}
func (a *workflowOutputArtifacts) OpenOutput(ctx context.Context, in agent.OutputArtifactRead) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.Binding != a.input.Binding || in.Ref.WorkflowRunID != a.input.Binding.WorkflowRunID || in.Ref.SessionID != "" {
		return nil, product.NewError(product.CodePermissionDenied, "output belongs to another run")
	}
	return io.NopCloser(strings.NewReader(a.input.Content)), nil
}
func TestWorkflowProcessOutputArtifactUsesRealWorkflowRoot(t *testing.T) {
	var count atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &count)
	process := &workflowProcess{}
	opts.Tools[0].Run = nil
	opts.Tools[0].Execution.BackendID = "process-operations"
	opts.Tools[0].Execution.Argv = []string{"synthetic"}
	opts.Operations.Process = process
	artifacts := &workflowOutputArtifacts{Memory: fixture.NewMemory()}
	opts.Operations.Artifacts = artifacts
	opts.Operations.OutputRedactor = func(_ context.Context, s string) (string, error) { return s, nil }
	w := newWorkflow(t, opts)
	submit(t, w)
	s := waitStopped(t, w)
	if s.State != "completed" || process.calls.Load() != 1 || artifacts.saves != 1 {
		t.Fatalf("output state=%s process=%d saves=%d", s.State, process.calls.Load(), artifacts.saves)
	}
	if artifacts.input.Binding.WorkflowRunID != s.RunID || artifacts.input.Binding.SessionID != "" {
		t.Fatalf("false output root %+v", artifacts.input.Binding)
	}
	w.mu.Lock()
	var projection *agent.ToolOutputProjection
	for _, c := range w.state.Calls {
		projection = c.Projection
	}
	w.mu.Unlock()
	if projection == nil || projection.Artifact.WorkflowRunID != s.RunID || projection.Artifact.SessionID != "" || projection.Artifact.ID == "" {
		t.Fatalf("artifact %+v", projection)
	}
	reader, err := artifacts.OpenOutput(t.Context(), agent.OutputArtifactRead{Binding: artifacts.input.Binding, Ref: projection.Artifact})
	if err != nil {
		t.Fatal(err)
	}
	full, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || len(full) != int(projection.Artifact.Size) {
		t.Fatal("full workflow output unavailable", err)
	}
	wrong := artifacts.input.Binding
	wrong.WorkflowRunID = "another"
	_, err = artifacts.OpenOutput(t.Context(), agent.OutputArtifactRead{Binding: wrong, Ref: projection.Artifact})
	requireCode(t, err, product.CodePermissionDenied)
	var output struct{ Result string }
	json.Unmarshal(s.Result, &output)
	if !strings.Contains(output.Result, `"workflowRunId":"`+s.RunID+`"`) {
		t.Fatal("returned result omitted workflow-owned reference")
	}
}
