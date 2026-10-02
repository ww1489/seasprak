package codeagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type capabilitySessionProcess struct {
	commandProcessProbe
	report atomic.Pointer[agent.BackendCapabilities]
}

func (p *capabilitySessionProcess) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	r := *p.report.Load()
	r.SupportedModes = append([]string(nil), r.SupportedModes...)
	return r, nil
}

type capabilitySessionFiles struct {
	agent.FileOperations
	report *capabilitySessionProcess
}

func (f capabilitySessionFiles) ExecutionCapabilities(ctx context.Context) (agent.BackendCapabilities, error) {
	return f.report.ExecutionCapabilities(ctx)
}

type missingSessionProcessReport struct{ agent.ProcessOperations }
type missingSessionFileReport struct{ agent.FileOperations }

func TestBackendCapabilitiesConfigurationGate(t *testing.T) {
	for _, backend := range []string{"files", "process"} {
		for _, kind := range []string{"missing", "unprotected", "unsupported-mode", "none", "unavailable", "partial", "full"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				report := sessionFakeCapabilities("configuration-fake")
				switch kind {
				case "unprotected":
					report.RuntimeDataWriteProtected = false
				case "unsupported-mode":
					report.SupportedModes = []string{"read-only"}
				case "none", "unavailable", "full":
					report.Enforcement = kind
				}
				process := &capabilitySessionProcess{}
				process.report.Store(&report)
				operations := tools.Operations{}
				if backend == "files" {
					operations.Files = capabilitySessionFiles{report: process}
					if kind == "missing" {
						operations.Files = missingSessionFileReport{operations.Files}
					}
				} else {
					operations.Process = process
					if kind == "missing" {
						operations.Process = missingSessionProcessReport{process}
					}
				}
				backendStore, err := memory.Open("capability-config", store.Header{})
				if err != nil {
					t.Fatal(err)
				}
				defer backendStore.Close()
				manager, err := state.NewManager(backendStore, "capability-config")
				if err != nil {
					t.Fatal(err)
				}
				s, err := Start(Options{SessionID: "capability-config", Profile: ProfileMemory, Model: testkit.NewFake(), Store: backendStore, Operations: operations}, manager, "gen")
				if kind == "partial" || kind == "full" {
					if err != nil {
						t.Fatal(err)
					}
					_ = s.Close(context.Background())
					return
				}
				if s != nil {
					_ = s.Close(context.Background())
					t.Fatal("unsafe configuration started")
				}
				requireSessionCode(t, err, product.CodeResourceUnavailable)
				if manager.View().LastSeq != 0 || process.calls.Load() != 0 {
					t.Fatal("rejected configuration committed policy or executed")
				}
			})
		}
	}
}

func TestBackendCapabilitiesReadOnlyBrowseNeedsNoReport(t *testing.T) {
	backend, err := memory.Open("capability-browse", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "capability-browse")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Start(Options{SessionID: "capability-browse", ReadOnly: true, Store: backend, Operations: tools.Operations{Process: missingSessionProcessReport{&commandProcessProbe{}}}}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if manager.View().LastSeq != 0 {
		t.Fatal("read-only browsing committed configuration")
	}
}

func TestBackendCapabilitiesSessionUsesCurrentModeAndRechecksBeforeClaim(t *testing.T) {
	for _, mode := range []string{"read-only", "workspace-write", "danger-full-access"} {
		for _, revoke := range []bool{false, true} {
			t.Run(mode+map[bool]string{true: "/revoked", false: "/protected"}[revoke], func(t *testing.T) {
				report := sessionFakeCapabilities("mode-fake")
				report.SupportedModes = []string{mode}
				process := &capabilitySessionProcess{}
				process.report.Store(&report)
				def := tools.Definition{Name: "probe", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "process-operations", Effect: "read", Argv: []string{"memory-only"}}}
				def.BeforeCall = []func(context.Context, agent.FrozenExecution) error{func(_ context.Context, f agent.FrozenExecution) error {
					if f.SandboxMode != mode || f.BackendCapabilitiesHash == "" {
						return product.NewError(product.CodeStateConflict, "mode/capability binding missing")
					}
					if revoke {
						changed := report
						changed.RuntimeDataWriteProtected = false
						process.report.Store(&changed)
					}
					return nil
				}}
				s, err := CreateAgentSession(t.Context(), Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "probe", Name: "probe", Arguments: `{}`}}}, testkit.Step{Text: "done"}), Policy: &agent.ResolvedPolicy{SandboxMode: mode}, Tools: []tools.Definition{def}, Operations: tools.Operations{Process: process}, ResourceScheduler: tools.NewResourceScheduler()})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close(context.Background())
				receipt := submitOutput(t, s)
				waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[receipt.TraceID].State) })
				v := s.rt.manager.View()
				call := onlyControlledCall(t, s)
				if revoke {
					if process.calls.Load() != 0 || call.Claimed || call.Observation == nil || call.Observation.Executed || call.Observation.Status != "denied" || !strings.Contains(call.Observation.Content, product.CodeResourceUnavailable) || len(v.ApprovalClaims) != 0 {
						t.Fatalf("revoked capability ran: calls=%d call=%+v", process.calls.Load(), call)
					}
				} else if process.calls.Load() != 1 || !call.Claimed || call.Observation == nil || call.Observation.Status != "succeeded" {
					t.Fatal("supported mode did not run exactly once")
				}
			})
		}
	}
}

func TestBackendCapabilitiesApprovalDiskReopenRejectsChangedAssembly(t *testing.T) {
	s, opts, originalProcess, callID, trace := capabilityApprovalFixture(t, true)
	answerCommand(t, s, "allowed-once")
	original := s.rt.manager.View().FrozenExecutions["execution:"+callID]
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	process := &capabilitySessionProcess{}
	report := sessionFakeCapabilities("command-process-fake")
	report.Version = "replaced-v2"
	process.report.Store(&report)
	opts.Operations.Process = process
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close(context.Background())
	if originalProcess.calls.Load() != 0 || process.calls.Load() != 0 {
		t.Fatal("opening resumed execution")
	}
	if snap := approvalSnapshot(t, opened); len(snap.Interactions) != 0 || len(snap.Approvals) != 0 {
		t.Fatal("Open asked before explicit Resume")
	}
	_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: trace, ExpectedRevision: opened.rt.manager.View().LastSeq})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(opened.rt.manager.View().Traces[trace].State) })
	v := opened.rt.manager.View()
	if originalProcess.calls.Load() != 0 || process.calls.Load() != 0 || len(v.ApprovalClaims) != 0 || v.Calls[callID].Claimed || v.Calls[callID].Observation == nil || v.Calls[callID].Observation.Executed || v.Traces[trace].Usage.ToolExecutions != 0 || len(approvalSnapshot(t, opened).Interactions) != 0 || v.Traces[trace].State != "failed" || !strings.Contains(v.Traces[trace].Error, product.CodeResourceUnavailable) || v.FrozenExecutions[original.ID].Hash != original.Hash {
		t.Fatal("disk reopen did not reject changed backend before asking")
	}
}

func TestBackendCapabilitiesApprovalResumeRejectsChangedAssembly(t *testing.T) {
	for _, field := range []string{"backend", "version", "environment", "protection"} {
		t.Run(field, func(t *testing.T) {
			report := sessionFakeCapabilities("approval-capability-fake")
			process := &capabilitySessionProcess{}
			process.report.Store(&report)
			def := builtinDefinitionForSession(t, "execute")
			def.Execution.RequestedGrantRef = "requires-approval"
			s, err := CreateAgentSession(t.Context(), Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Principal: "host", GenerationFingerprint: "capability-approval-v1", Model: versionedPauseModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "execute", Name: "execute", Arguments: `{"argv":["echo","hi"],"cwd":"workspace"}`}}}, testkit.Step{Text: "done"})}, Tools: []tools.Definition{def}, Operations: tools.Operations{Process: process}, ResourceScheduler: tools.NewResourceScheduler()})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			receipt := submitOutput(t, s)
			trace := receipt.TraceID
			waitResumeCondition(t, func() bool {
				tr := s.rt.manager.View().Traces[trace]
				return tr.State == "paused" || terminal(tr.State)
			})
			v := s.rt.manager.View()
			if v.Traces[trace].State != "paused" {
				t.Fatal("model tool did not await approval")
			}
			callID := onlyControlledCall(t, s).Call.CallID
			original := v.FrozenExecutions["execution:"+callID]
			answerCommand(t, s, "allowed-once")
			switch field {
			case "backend":
				report.BackendID = "other-backend"
			case "version":
				report.Version = "other-version"
			case "environment":
				report.EnvironmentID = "other-environment"
			case "protection":
				report.RuntimeDataWriteProtected = false
			}
			process.report.Store(&report)
			_, err = s.Resume(t.Context(), ResumeCommand{TraceID: trace, ExpectedRevision: s.rt.manager.View().LastSeq})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[trace].State) })
			v = s.rt.manager.View()
			if process.calls.Load() != 0 || len(v.ApprovalClaims) != 0 || v.Calls[callID].Claimed || v.Calls[callID].Observation == nil || v.Calls[callID].Observation.Executed || v.FrozenExecutions[original.ID].Hash != original.Hash {
				t.Fatal("changed assembly reused old approval")
			}
			want := product.CodeStateConflict
			if field == "protection" {
				want = product.CodeResourceUnavailable
			}
			if !strings.Contains(v.Calls[callID].Observation.Content+v.Traces[trace].Error, want) {
				t.Fatalf("tool error=%s want=%s", v.Calls[callID].Observation.Content, want)
			}
		})
	}
}
