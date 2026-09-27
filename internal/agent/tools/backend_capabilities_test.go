package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

// Expose only the operation port, deliberately hiding any capability reporter.
type unreportedProcess struct{ agent.ProcessOperations }
type unreportedFiles struct{ agent.FileOperations }

type changingCapabilities struct {
	report atomic.Pointer[agent.BackendCapabilities]
	err    error
}

func (p *changingCapabilities) set(report agent.BackendCapabilities) { p.report.Store(&report) }
func (p *changingCapabilities) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	if p.err != nil {
		return agent.BackendCapabilities{}, p.err
	}
	value := *p.report.Load()
	value.SupportedModes = append([]string(nil), value.SupportedModes...)
	return value, nil
}

type reportedProcess struct {
	agent.ProcessOperations
	*changingCapabilities
}
type reportedFiles struct {
	agent.FileOperations
	*changingCapabilities
}

func TestBackendCapabilitiesRequiredMatrix(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*agent.BackendCapabilities)
		allowed bool
	}{
		{"partial-protected", func(*agent.BackendCapabilities) {}, true},
		{"full-protected", func(r *agent.BackendCapabilities) { r.Enforcement = "full" }, true},
		{"partial-unprotected", func(r *agent.BackendCapabilities) { r.RuntimeDataWriteProtected = false }, false},
		{"full-unprotected", func(r *agent.BackendCapabilities) { r.Enforcement = "full"; r.RuntimeDataWriteProtected = false }, false},
		{"unsupported-mode", func(r *agent.BackendCapabilities) { r.SupportedModes = []string{"read-only"} }, false},
		{"missing-modes", func(r *agent.BackendCapabilities) { r.SupportedModes = nil }, false},
		{"none", func(r *agent.BackendCapabilities) { r.Enforcement = "none" }, false},
		{"unavailable", func(r *agent.BackendCapabilities) { r.Enforcement = "unavailable" }, false},
		{"unknown-enforcement", func(r *agent.BackendCapabilities) { r.Enforcement = "native" }, false},
		{"missing-backend", func(r *agent.BackendCapabilities) { r.BackendID = "" }, false},
		{"missing-version", func(r *agent.BackendCapabilities) { r.Version = "" }, false},
		{"missing-environment", func(r *agent.BackendCapabilities) { r.EnvironmentID = "" }, false},
	}
	for _, backend := range []string{"file-operations", "process-operations"} {
		for _, tc := range cases {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				report := memoryBackendCapabilities("matrix-fake")
				tc.change(&report)
				caps := &changingCapabilities{}
				caps.set(report)
				process, files := &controlledProcessOperations{}, &controlledFileOperations{}
				operations := Operations{Process: reportedProcess{process, caps}, Files: reportedFiles{files, caps}}
				args := `{"operation":"write","path":"file","contentRef":"content"}`
				sink := &recordSink{found: true, rec: accepted(args)}
				def := Definition{Name: "add", Version: "1", Schema: []byte(`{"type":"object"}`), Execution: ExecutionDescription{BackendID: backend, Effect: "write"}}
				if backend == "process-operations" {
					def.Execution.Argv = []string{"fake"}
				}
				exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(operations), WithResourceScheduler(NewResourceScheduler()))
				if err != nil {
					t.Fatal(err)
				}
				out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", args)
				if tc.allowed {
					frozenCommitted := false
					for _, fact := range sink.facts {
						if fact.Kind == "tool_frozen" {
							frozenCommitted = true
						}
					}
					if !frozenCommitted {
						t.Fatal("controlled capability binding was not committed")
					}
					if err != nil || !out.Executed || !hasIntent(sink) || process.starts.Load()+files.writes.Load() != 1 {
						t.Fatalf("expected controlled execution: out=%+v err=%v", out, err)
					}
				} else {
					pe, ok := product.AsError(err)
					if !ok || pe.Code != product.CodeResourceUnavailable || out.Executed || hasIntent(sink) || process.starts.Load()+files.writes.Load() != 0 {
						t.Fatalf("unsafe execution: out=%+v err=%v", out, err)
					}
				}
			})
		}
	}
}

func TestBackendCapabilitiesChangedAfterFreezePreventsClaim(t *testing.T) {
	for _, field := range []string{"backend", "version", "environment", "modes", "enforcement", "protection"} {
		t.Run(field, func(t *testing.T) {
			caps := &changingCapabilities{}
			report := memoryBackendCapabilities("changing-fake")
			caps.set(report)
			process := &controlledProcessOperations{}
			sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
			def := addDef(nil)
			def.Execution = ExecutionDescription{BackendID: "process-operations", Effect: "write", Argv: []string{"fake"}}
			def.BeforeCall = []func(context.Context, agent.FrozenExecution) error{func(_ context.Context, frozen agent.FrozenExecution) error {
				if frozen.BackendCapabilitiesHash == "" || frozen.SandboxMode != "workspace-write" {
					t.Fatal("capabilities were not frozen")
				}
				copy := frozen
				copy.BackendCapabilitiesHash = "changed"
				hash, _ := copy.Digest()
				if hash == frozen.Hash {
					t.Fatal("capabilities excluded from digest")
				}
				switch field {
				case "backend":
					report.BackendID = "replacement"
				case "version":
					report.Version = "v2"
				case "environment":
					report.EnvironmentID = "replacement"
				case "modes":
					report.SupportedModes = []string{"workspace-write"}
				case "enforcement":
					report.Enforcement = "full"
				case "protection":
					report.RuntimeDataWriteProtected = false
				}
				caps.set(report)
				return nil
			}}
			exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Process: reportedProcess{process, caps}}), WithResourceScheduler(NewResourceScheduler()))
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
			pe, ok := product.AsError(err)
			if !ok || pe.Code != product.CodeResourceUnavailable || hasIntent(sink) || out.Executed || process.starts.Load() != 0 {
				t.Fatalf("changed capabilities executed: %+v %v", out, err)
			}
		})
	}
}

type capabilityClaimSink struct {
	*recordSink
	afterClaim func()
}

func (s capabilityClaimSink) CommitFact(ctx context.Context, scope agent.ExecutionScope, fact agent.Fact) error {
	err := s.recordSink.CommitFact(ctx, scope, fact)
	if err == nil && fact.Kind == "tool_intent" {
		s.afterClaim()
	}
	return err
}

func TestBackendCapabilitiesChangedAfterClaimPreventsInvocation(t *testing.T) {
	caps := &changingCapabilities{}
	report := memoryBackendCapabilities("claim-fake")
	caps.set(report)
	process := &controlledProcessOperations{}
	record := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	sink := capabilityClaimSink{record, func() { report.Version = "v2"; caps.set(report) }}
	def := addDef(nil)
	def.Execution = ExecutionDescription{BackendID: "process-operations", Effect: "write", Argv: []string{"fake"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Process: reportedProcess{process, caps}}), WithResourceScheduler(NewResourceScheduler()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeResourceUnavailable || !hasIntent(record) || out.Executed || process.validates.Load() != 0 || process.starts.Load() != 0 {
		t.Fatalf("changed post-claim backend invoked: %+v %v", out, err)
	}
}

func TestBackendCapabilitiesPostClaimObservationFailureRetainsHold(t *testing.T) {
	caps := &changingCapabilities{}
	report := memoryBackendCapabilities("failed-observation-fake")
	caps.set(report)
	process := &controlledProcessOperations{}
	record := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	storeErr := product.NewError(product.CodeStorageUnavailable, "controlled observation failure")
	record.beforeCommit = func(f agent.Fact) error {
		if f.Kind == "tool_observation" {
			return storeErr
		}
		return nil
	}
	sink := capabilityClaimSink{record, func() { report.Version = "v2"; caps.set(report) }}
	scheduler := NewResourceScheduler()
	def := addDef(nil)
	def.Execution = ExecutionDescription{BackendID: "process-operations", Effect: "write", Argv: []string{"fake"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Process: reportedProcess{process, caps}}), WithResourceScheduler(scheduler), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	if !errors.Is(err, storeErr) || out.Executed || !hasIntent(record) || process.starts.Load() != 0 || !scheduler.HasHold(ResourceHoldID("session", "prod-1")) {
		t.Fatalf("unresolved durable claim lost resource hold: out=%+v err=%v", out, err)
	}
}

func TestBackendCapabilitiesChangedWhileResourceWaitingPreventsClaim(t *testing.T) {
	scheduler := NewResourceScheduler()
	lease, err := scheduler.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Effect: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	caps := &changingCapabilities{}
	report := memoryBackendCapabilities("waiting-fake")
	caps.set(report)
	process := &controlledProcessOperations{}
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	def := addDef(nil)
	def.Execution = ExecutionDescription{BackendID: "process-operations", Effect: "write", Argv: []string{"fake"}}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Process: reportedProcess{process, caps}}), WithResourceScheduler(scheduler), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var out Outcome
	var runErr error
	go func() {
		defer close(done)
		out, runErr = exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", `{"n":1}`)
	}()
	waitResourceQueue(t, scheduler, 1)
	report.RuntimeDataWriteProtected = false
	caps.set(report)
	lease.Release()
	<-done
	pe, ok := product.AsError(runErr)
	if !ok || pe.Code != product.CodeResourceUnavailable || hasIntent(sink) || out.Executed || process.starts.Load() != 0 {
		t.Fatalf("waiting execution claimed after revocation: %+v %v", out, runErr)
	}
}

func TestBackendCapabilitiesTicketRechecksAfterUpstreamValidator(t *testing.T) {
	for _, backend := range []string{"file-operations", "process-operations"} {
		t.Run(backend, func(t *testing.T) {
			report := memoryBackendCapabilities("ticket-fake")
			caps := &changingCapabilities{}
			caps.set(report)
			process, files := &controlledProcessOperations{}, &controlledFileOperations{}
			args := `{"operation":"write","path":"file","contentRef":"content"}`
			sink := &recordSink{found: true, rec: accepted(args)}
			var validations int
			sink.validator = testTicketValidator(func(context.Context, agent.FrozenExecution) error {
				validations++
				report.RuntimeDataWriteProtected = false
				caps.set(report)
				return nil
			})
			def := Definition{Name: "add", Version: "1", Schema: []byte(`{"type":"object"}`), Execution: ExecutionDescription{BackendID: backend, Effect: "write"}}
			if backend == "process-operations" {
				def.Execution.Argv = []string{"fake"}
			}
			exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Process: reportedProcess{process, caps}, Files: reportedFiles{files, caps}}), WithResourceScheduler(NewResourceScheduler()))
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", args)
			if err != nil || out.Executed || validations != 1 || !hasIntent(sink) || process.starts.Load() != 0 || files.writes.Load() != 0 {
				t.Fatalf("ticket accepted changed capability: out=%+v err=%v validations=%d", out, err, validations)
			}
		})
	}
}

func TestBackendCapabilitiesProbeFailureIsUnavailableAndSanitized(t *testing.T) {
	caps := &changingCapabilities{err: errors.New("private-backend-diagnostic")}
	_, err := (Operations{Process: reportedProcess{&controlledProcessOperations{}, caps}}).CapabilityHash(t.Context(), "process-operations", "workspace-write")
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeResourceUnavailable || strings.Contains(err.Error(), "private-backend-diagnostic") {
		t.Fatalf("unexpected probe error: %v", err)
	}
}

func TestBackendCapabilitiesMissingReportPreventsClaim(t *testing.T) {
	for _, backend := range []string{"process-operations", "file-operations"} {
		t.Run(backend, func(t *testing.T) {
			process := &controlledProcessOperations{}
			files := &controlledFileOperations{}
			operations := Operations{Process: unreportedProcess{process}, Files: unreportedFiles{files}}
			args := `{"operation":"write","path":"file","contentRef":"content"}`
			sink := &recordSink{found: true, rec: accepted(args)}
			def := addDef(func(context.Context, json.RawMessage) (string, error) {
				t.Fatal("missing capability report fell back to trusted Run")
				return "", nil
			})
			def.Schema = []byte(`{"type":"object"}`)
			def.Execution = ExecutionDescription{BackendID: backend, Effect: "write"}
			if backend == "process-operations" {
				def.Execution.Argv = []string{"fake"}
			}
			exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(operations), WithResourceScheduler(NewResourceScheduler()))
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "prov-1", "add", args)
			pe, ok := product.AsError(err)
			if !ok || pe.Code != product.CodeResourceUnavailable || out.Executed || hasIntent(sink) || process.starts.Load() != 0 || files.writes.Load() != 0 {
				t.Fatalf("err=%v outcome=%+v intent=%v process=%d files=%d", err, out, hasIntent(sink), process.starts.Load(), files.writes.Load())
			}
		})
	}
}
