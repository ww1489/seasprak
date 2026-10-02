package state_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

func childResumeFixture(t *testing.T, stopped bool) (*state.Manager, *failingStore, state.Invocation) {
	t.Helper()
	m, s := fixture(t)
	in := accept(t, m, "child-input", `{"text":"delegate"}`)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.SetTraceState(t.Context(), in.TraceID, "running", false))
	must(m.Consume(t.Context(), in.InputID))
	v := m.View()
	tr := v.Traces[in.TraceID]
	scope := agent.ExecutionScope{SessionID: "session", BranchID: v.BranchID, TraceID: tr.ID, InvocationID: tr.InvocationID, ExecutionID: "original", Generation: "g", TurnID: "turn"}
	must(m.SaveTurn(t.Context(), agent.TurnRecord{ID: "turn", TraceID: tr.ID, InvocationID: tr.InvocationID}))
	call := agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: "delegate", ProviderCallID: "provider", Name: "delegate_task", Arguments: `{"agent":"worker","task":"work"}`, Generation: "g"}}
	msg := agent.AgentMessage{ID: "assistant", Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: "session", TraceID: tr.ID, InvocationID: tr.InvocationID, TurnID: "turn"}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider", Name: "delegate_task", Arguments: call.Call.Arguments})}}}
	must(m.SaveAssistant(t.Context(), msg, []agent.ToolRecord{call}))
	call.Claimed = true
	must(m.SaveCall(t.Context(), call))
	inv := state.Invocation{ID: "child", ParentInvocationID: tr.InvocationID, ParentCallID: call.Call.CallID, TraceID: tr.ID, Target: agent.TargetAgent{Name: "worker", Version: "v1", Generation: "g"}, State: "running", RecoveryFingerprint: "compatible", RecoveryLeafID: m.View().LeafID, RecoveryInputID: in.InputID}
	must(m.SaveInvocation(t.Context(), inv))
	childScope := scope
	childScope.InvocationID, childScope.ParentInvocationID, childScope.TurnID = inv.ID, inv.ParentInvocationID, ""
	must(m.RegisterChildCalls(t.Context(), inv.ID, []agent.ToolRecord{{Scope: childScope, Call: agent.FrozenCall{CallID: "old-child-call", ProviderCallID: "old-provider", Name: "probe", Arguments: `{}`, Generation: "g"}}}))
	inv = m.View().Invocations[inv.ID]
	inv.State = "interrupted"
	must(m.SaveInvocation(t.Context(), inv))
	if stopped {
		must(m.ConfirmExecutionStopped(t.Context(), tr.ID))
	}
	must(m.SetTraceState(t.Context(), tr.ID, "paused", false))
	return m, s, inv
}

func resumeChildCommand(m *state.Manager, inv state.Invocation) state.OperationCommand {
	return state.OperationCommand{Principal: "local", Kind: "resume", Target: inv.TraceID, ExpectedRevision: m.View().LastSeq, IdempotencyKey: "resume-child"}
}

func TestChildResumeCommitFailureIsAtomic(t *testing.T) {
	m, s, inv := childResumeFixture(t, true)
	before := m.View()
	s.fail = true
	_, err := m.CommitChildResume(t.Context(), resumeChildCommand(m, inv), inv.ID, "recovered")
	if err == nil || !reflect.DeepEqual(before, m.View()) {
		t.Fatalf("failed acceptance published state: %v", err)
	}
}

func TestChildResumeRequiresStoppedParent(t *testing.T) {
	m, _, inv := childResumeFixture(t, false)
	before := m.View()
	_, err := m.CommitChildResume(t.Context(), resumeChildCommand(m, inv), inv.ID, "recovered")
	requireP2Code(t, err, product.CodeIncompatibleResume)
	if !reflect.DeepEqual(before, m.View()) {
		t.Fatal("rejected restart changed state")
	}
}

func TestChildResumeDuplicateReusesAcceptance(t *testing.T) {
	m, _, inv := childResumeFixture(t, true)
	cmd := resumeChildCommand(m, inv)
	first, err := m.CommitChildResume(t.Context(), cmd, inv.ID, "recovered")
	if err != nil {
		t.Fatal(err)
	}
	before := m.View()
	second, err := m.CommitChildResume(t.Context(), cmd, inv.ID, "different")
	if err != nil || first != second || !reflect.DeepEqual(before, m.View()) {
		t.Fatalf("duplicate acceptance changed identity: %v", err)
	}
}

func TestChildResumeCompletionFailureIsAtomic(t *testing.T) {
	m, s, inv := childResumeFixture(t, true)
	if _, err := m.CommitChildResume(t.Context(), resumeChildCommand(m, inv), inv.ID, "recovered"); err != nil {
		t.Fatal(err)
	}
	before := m.View()
	s.fail = true
	if err := m.CompleteChildResume(t.Context(), inv.ID, "recovered", "done", 1, "", false); err == nil || !reflect.DeepEqual(before, m.View()) {
		t.Fatalf("completion published partial state: %v", err)
	}
}

func TestChildResumeCompletionCannotHideUnknownEffect(t *testing.T) {
	m, _, inv := childResumeFixture(t, true)
	if _, err := m.CommitChildResume(t.Context(), resumeChildCommand(m, inv), inv.ID, "recovered"); err != nil {
		t.Fatal(err)
	}
	scope := m.View().ResumedExecutions["recovered"].Scope
	scope.InvocationID, scope.ParentInvocationID = inv.ID, inv.ParentInvocationID
	call := agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: "child-tool", ProviderCallID: "tool", Name: "effect", Generation: "g"}}
	if err := m.RegisterChildCalls(t.Context(), inv.ID, []agent.ToolRecord{call}); err != nil {
		t.Fatal(err)
	}
	call.Claimed = true
	call.Observation = &agent.ToolObservation{Status: "outcome_unknown", SideEffect: "unknown", Executed: true}
	if err := m.SaveCall(t.Context(), call); err != nil {
		t.Fatal(err)
	}
	before := m.View()
	err := m.CompleteChildResume(t.Context(), inv.ID, "recovered", "done", 1, "", false)
	requireP2Code(t, err, product.CodeReconciliationRequired)
	if !reflect.DeepEqual(before, m.View()) {
		t.Fatal("unknown completion changed state")
	}
}

func TestChildResumeCompletionReplayRejectsPartialCommit(t *testing.T) {
	for _, kind := range []string{"missing-invocation", "missing-parent", "missing-turn", "missing-results"} {
		t.Run(kind, func(t *testing.T) {
			m, s, inv := childResumeFixture(t, true)
			if _, err := m.CommitChildResume(t.Context(), resumeChildCommand(m, inv), inv.ID, "recovered"); err != nil {
				t.Fatal(err)
			}
			if err := m.CompleteChildResume(t.Context(), inv.ID, "recovered", "done", 1, "", false); err != nil {
				t.Fatal(err)
			}
			corrupt := childResumeMutationStore{Store: s, mutate: func(c *store.Commit) {
				var records []store.Record
				for _, r := range c.ControlRecords {
					if kind == "missing-invocation" && r.Type == "invocation" || kind == "missing-parent" && r.Type == "tool_call" || kind == "missing-turn" && r.Type == "turn" {
						continue
					}
					records = append(records, r)
				}
				c.ControlRecords = records
				if kind == "missing-results" {
					c.Entries = nil
				}
			}}
			if _, err := state.NewManager(corrupt, "session"); err == nil {
				t.Fatal("partial child completion accepted on replay")
			}
		})
	}
}

// Load-only mutations deliberately bypass storage checks to exercise the
// state's whole-commit validation independently of the journal backend.
type childResumeMutationStore struct {
	store.Store
	mutate func(*store.Commit)
}

func (s childResumeMutationStore) Load(ctx context.Context, id string) (store.StoredSession, error) {
	saved, err := s.Store.Load(ctx, id)
	if err == nil {
		s.mutate(&saved.Commits[len(saved.Commits)-1])
	}
	return saved, err
}

func TestChildResumeReplayRejectsPartialAndWrongScopeCommit(t *testing.T) {
	for _, kind := range []string{"missing-trace", "missing-invocation", "missing-resumed-execution", "missing-old-call", "changed-old-arguments", "changed-old-scope", "wrong-session", "wrong-branch"} {
		t.Run(kind, func(t *testing.T) {
			m, s, inv := childResumeFixture(t, true)
			if _, err := m.CommitChildResume(t.Context(), resumeChildCommand(m, inv), inv.ID, "recovered"); err != nil {
				t.Fatal(err)
			}
			corrupt := childResumeMutationStore{Store: s, mutate: func(c *store.Commit) {
				var records []store.Record
				for _, r := range c.ControlRecords {
					if kind == "missing-trace" && r.Type == "trace" || kind == "missing-invocation" && r.Type == "invocation" || kind == "missing-resumed-execution" && r.Type == "resumed_execution" || kind == "missing-old-call" && r.Type == "tool_call" {
						continue
					}
					if r.Type == "tool_call" {
						var call agent.ToolRecord
						_ = json.Unmarshal(r.Payload, &call)
						if kind == "changed-old-arguments" {
							call.Call.Arguments = `{"changed":true}`
						}
						if kind == "changed-old-scope" {
							call.Scope.ExecutionID = "another-execution"
						}
						r.Payload, _ = json.Marshal(call)
					}
					if r.Type == "resumed_execution" {
						var segment state.ResumedExecution
						_ = json.Unmarshal(r.Payload, &segment)
						if kind == "wrong-session" {
							segment.Scope.SessionID = "other"
						}
						if kind == "wrong-branch" {
							segment.Scope.BranchID = "other"
						}
						r.Payload, _ = json.Marshal(segment)
					}
					records = append(records, r)
				}
				c.ControlRecords = records
			}}
			if _, err := state.NewManager(corrupt, "session"); err == nil {
				t.Fatal("unsafe resumed commit accepted on replay")
			}
		})
	}
}
