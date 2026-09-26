package sessions

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2PolicySessionExecution(t *testing.T) {
	for _, tc := range []struct {
		name        string
		policy      *agent.ResolvedPolicy
		description tools.ExecutionDescription
		code        string
		run         bool
	}{
		{name: "default-trusted", run: true},
		{name: "default-explicit-trusted", description: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "write"}, run: true},
		{name: "missing-backend", description: tools.ExecutionDescription{BackendID: "memory-files"}, code: product.CodeResourceUnavailable},
		{name: "trusted-name-with-process", description: tools.ExecutionDescription{BackendID: "trusted-run", Argv: []string{"command"}}, code: product.CodeResourceUnavailable},
		{name: "unavailable-full-backend", policy: &agent.ResolvedPolicy{SandboxMode: "danger-full-access"}, description: tools.ExecutionDescription{BackendID: "native"}, code: product.CodeResourceUnavailable},
		{name: "ask-unimplemented", description: tools.ExecutionDescription{RequestedGrantRef: "request"}, code: product.CodeResourceUnavailable},
		{name: "never-is-not-allow", policy: &agent.ResolvedPolicy{ApprovalPolicy: "never"}, description: tools.ExecutionDescription{RequestedGrantRef: "request"}, code: product.CodePermissionDenied},
		{name: "read-only-write", policy: &agent.ResolvedPolicy{SandboxMode: "read-only"}, description: tools.ExecutionDescription{Effect: "write"}, code: product.CodePermissionDenied},
		{name: "read-only-unknown", policy: &agent.ResolvedPolicy{SandboxMode: "read-only"}, code: product.CodePermissionDenied},
		{name: "read-only-declared-read", policy: &agent.ResolvedPolicy{SandboxMode: "read-only"}, description: tools.ExecutionDescription{Effect: "read"}, run: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, err := memory.Open("policy", store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			manager, err := state.NewManager(backend, "policy")
			if err != nil {
				t.Fatal(err)
			}
			gate := make(chan struct{})
			model := testkit.NewFake(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "done"})
			var runs atomic.Int32
			opts := Options{SessionID: "policy", Profile: ProfileMemory, Policy: tc.policy, Store: backend, Model: model, Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tc.description, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ran", nil }}}}
			if _, err := alignTools(&opts); err != nil {
				t.Fatal(err)
			}
			s, err := Start(opts, manager, "gen")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"work"}`)})
			if err != nil {
				t.Fatal(err)
			}
			frame := activityFrame(t, s)
			close(gate)
			activityWait(t, frame)
			v := manager.View()
			tr := v.Traces[receipt.TraceID]
			wantRuns, wantState, wantStatus := int32(0), "failed", "denied"
			if tc.run {
				wantRuns, wantState, wantStatus = 1, "completed", "succeeded"
			}
			if runs.Load() != wantRuns || tr.Usage.ToolExecutions != int(wantRuns) || tr.State != wantState || (tc.code != "" && !strings.Contains(tr.Error, tc.code)) {
				t.Fatalf("runs=%d trace=%+v", runs.Load(), tr)
			}
			if len(v.Calls) != 1 || len(v.FrozenExecutions) != 1 {
				t.Fatal("missing accepted/frozen call")
			}
			for _, call := range v.Calls {
				if call.Claimed != tc.run || call.Observation == nil || call.Observation.Executed != tc.run || call.Observation.Status != wantStatus || call.Observation.SideEffect != "none" {
					t.Fatalf("call=%+v observation=%+v", call, call.Observation)
				}
			}
			results := 0
			for _, msg := range v.Messages {
				if msg.Kind == agent.KindToolResult {
					results++
				}
			}
			if results != 1 {
				t.Fatalf("paired results=%d", results)
			}
			for _, event := range v.Events {
				if !tc.run && event.Type == "tool.started" {
					t.Fatal("denied call started")
				}
			}
		})
	}
}

func TestP2PolicyRejectsUnsupportedConfigurationBeforeStart(t *testing.T) {
	for _, tc := range []struct {
		policy agent.ResolvedPolicy
		code   string
	}{
		{agent.ResolvedPolicy{Auto: true}, product.CodeUnsupportedCapability},
		{agent.ResolvedPolicy{SandboxMode: "invalid"}, product.CodeInvalidArgument},
		{agent.ResolvedPolicy{ApprovalPolicy: "always"}, product.CodeInvalidArgument},
	} {
		backend, err := memory.Open("invalid-policy", store.Header{})
		if err != nil {
			t.Fatal(err)
		}
		manager, err := state.NewManager(backend, "invalid-policy")
		if err != nil {
			t.Fatal(err)
		}
		s, err := Start(Options{SessionID: "invalid-policy", Profile: ProfileMemory, Model: testkit.NewFake(), Store: backend, Policy: &tc.policy}, manager, "gen")
		if s != nil {
			s.Close(context.Background())
		}
		pe, ok := product.AsError(err)
		if !ok || pe.Code != tc.code || manager.View().LastSeq != 0 {
			t.Fatalf("err=%v commits=%d", err, manager.View().LastSeq)
		}
	}
}
