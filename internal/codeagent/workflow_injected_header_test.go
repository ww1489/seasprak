package codeagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type workflowHeaderAppendProbe struct {
	storage.Store
	appends atomic.Int32
}

func (p *workflowHeaderAppendProbe) Append(ctx context.Context, id string, expected storage.ExpectedCommit, commit storage.Commit) (storage.CommitReceipt, error) {
	p.appends.Add(1)
	return p.Store.Append(ctx, id, expected, commit)
}

func TestCodeCreateRejectsInjectedWorkflowHeaderBeforeAppend(t *testing.T) {
	const id = "injected-workflow-root"
	backend, err := memory.Open(id, storage.Header{ResourceType: storage.ResourceWorkflow, RunID: id, Workspace: json.RawMessage(`{"resourceType":"workflow","formatVersion":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	probe := &workflowHeaderAppendProbe{Store: backend}
	m := testkit.NewFake(testkit.Step{Text: "must not execute"})
	var toolCalls atomic.Int32
	s, err := CreateAgentSession(t.Context(), Options{
		Workspace: t.TempDir(), StateRoot: "memory", SessionID: id, Store: probe,
		Profile: ProfileMemory, Model: m, Principal: "local", GenerationFingerprint: "header-test-v1",
		Tools: []tools.Definition{{Name: "echo", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) {
			toolCalls.Add(1)
			return "forbidden", nil
		}}},
	})
	if s != nil {
		defer s.Close(context.Background())
		t.Error("Code factory returned a session backed by a Workflow journal")
	}
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
		t.Errorf("error=%v, want incompatible_version", err)
	}
	if probe.appends.Load() != 0 || m.Calls() != 0 || toolCalls.Load() != 0 {
		t.Errorf("cross-type creation effects: appends=%d model=%d tool=%d, want all zero", probe.appends.Load(), m.Calls(), toolCalls.Load())
	}
	loaded, err := backend.Load(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Header.ResourceType != storage.ResourceWorkflow || loaded.Header.RunID != id || loaded.Header.SessionID != "" || len(loaded.Commits) != 0 {
		t.Errorf("Workflow header-only resource changed: header=%+v commits=%d", loaded.Header, len(loaded.Commits))
	}
}
