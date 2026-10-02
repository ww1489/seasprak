package codeagent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

// This backend performs the version comparison at the actual write boundary,
// rather than treating a frozen expectedVersion as proof of conflict handling.
type approvalVersionFiles struct {
	agent.FileOperations
	mu               sync.Mutex
	version, content string
	calls, writes    int
	seenVersion      string
}

func (f *approvalVersionFiles) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return agent.BackendCapabilities{BackendID: "file-operations", Version: "test-v1", EnvironmentID: "approval-version-fixture", SupportedModes: []string{"workspace-write"}, Enforcement: "full", RuntimeDataWriteProtected: true}, nil
}

func (f *approvalVersionFiles) Write(ctx context.Context, request agent.AuthorizedFileWrite) (agent.FileEffect, error) {
	if err := request.Authorization.Validate(ctx); err != nil {
		return agent.FileEffect{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seenVersion = request.ExpectedVersion
	if request.ExpectedVersion != f.version {
		return agent.FileEffect{Identity: request.Path, Version: f.version, SideEffect: "none", Confirmed: true}, product.NewError(product.CodeStateConflict, "file version changed")
	}
	f.writes++
	f.content, f.version = request.ContentRef, "v3"
	return agent.FileEffect{Identity: request.Path, Version: f.version, SideEffect: "confirmed", Confirmed: true}, nil
}

func TestApprovalExternalFileVersionChangeDoesNotOverwrite(t *testing.T) {
	for _, kind := range []string{"invokable", "enhanced-invokable"} {
		t.Run(kind, func(t *testing.T) {
			files := &approvalVersionFiles{version: "v1", content: "original"}
			def := builtinDefinitionForSession(t, "write_file")
			def.ToolInterface = kind
			def.Execution.RequestedGrantRef = "requires-approval"
			model := versionedPauseModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "write_file", Arguments: `{"path":"document","contentRef":"replacement","expectedVersion":"v1"}`}}}, testkit.Step{Text: "finished"})}
			s, err := CreateAgentSession(t.Context(), Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Principal: "host", GenerationFingerprint: "file-version-approval", Model: model, Tools: []tools.Definition{def}, Operations: tools.Operations{Files: files}, ResourceScheduler: tools.NewResourceScheduler()})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"write"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool {
				tr := s.rt.manager.View().Traces[receipt.TraceID]
				return tr.State == "paused" || terminal(tr.State)
			})
			view := s.rt.manager.View()
			if view.Traces[receipt.TraceID].State != "paused" || len(view.FrozenExecutions) != 1 {
				t.Fatal("write did not await approval")
			}
			for _, frozen := range view.FrozenExecutions {
				if len(frozen.Resources) != 1 || frozen.Resources[0].ExpectedVersion != "v1" {
					t.Fatalf("missing frozen version: %+v", frozen.Resources)
				}
			}
			files.mu.Lock()
			if files.calls != 0 {
				t.Error("backend called before approval")
			}
			files.version, files.content = "v2", "external edit"
			files.mu.Unlock()
			answerCommand(t, s, "allowed-once")
			if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: receipt.TraceID, ExpectedRevision: s.rt.manager.View().LastSeq}); err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[receipt.TraceID].State) })
			files.mu.Lock()
			defer files.mu.Unlock()
			if files.calls != 1 || files.writes != 0 || files.seenVersion != "v1" || files.version != "v2" || files.content != "external edit" {
				t.Fatalf("conflict was not preserved: calls=%d writes=%d expected=%s version=%s content=%s", files.calls, files.writes, files.seenVersion, files.version, files.content)
			}
			view = s.rt.manager.View()
			if len(view.Calls) != 1 || view.Traces[receipt.TraceID].Usage.ToolExecutions != 1 {
				t.Fatal("wrong invocation count")
			}
			for _, call := range view.Calls {
				if !call.Claimed || call.Observation == nil || call.Observation.Status != "failed" || call.Observation.SideEffect != "none" {
					t.Fatalf("wrong conflict observation: %+v", call)
				}
			}
		})
	}
}
