package codeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

// This reads the real JSONL transaction at the offline transport entrance.
// It neither reserves a request nor creates an attempt on behalf of the product.
func p3AuxiliaryLimitWireReservation(t *testing.T, s *AgentSession, r *http.Request, logical, physical, perCall int, purpose string) {
	t.Helper()
	scope := einorun.ScopeFromContext(r.Context(), agent.ExecutionScope{})
	v := s.rt.manager.View()
	budget := v.InvocationBudgets[scope.InvocationID]
	request := budget.Usage.LastTransport
	attempt := v.ModelAttempts[request.AttemptID]
	parent := v.Traces[scope.TraceID]
	if scope.ParentInvocationID == "" || scope.TurnID != "" || scope.SelectionRevision != 0 || budget.Scope != scope || budget.Usage.LogicalModelCalls != logical || budget.Usage.TransportRequests != physical || budget.Usage.ModelRequests != perCall || budget.Usage.ToolExecutions != 0 || budget.Calls[budget.Usage.ModelCallID].Requests != perCall || request.ModelCallID != budget.Usage.ModelCallID || request.TransportAttempt != uint64(perCall) || request.Purpose != purpose || attempt.ID == "" || attempt.State != "started" || attempt.Scope != scope || attempt.ModelCallID != request.ModelCallID || attempt.Attempt != 1 {
		t.Error("offline wire preceded its original child attempt and exact independent reservation")
		return
	}
	if _, ended := v.AttemptResults[attempt.ID]; ended {
		t.Error("offline wire reused a terminal attempt")
	}
	// Child charges preserve the parent's original active Turn and, for an
	// observed parent, its original transport identity as well.
	if parent == nil {
		t.Error("child wire has no active parent trace")
		return
	}
	parentTurn := v.Turns[parent.Usage.ModelCallID]
	if parent.Usage.LogicalModelCalls != 1+logical || parent.Usage.TransportRequests != 1+physical || parent.Usage.ModelRequests != 1 || parent.Usage.ModelCallID == request.ModelCallID || parentTurn.TransportRequests != 1 || parentTurn.InvocationID != scope.ParentInvocationID {
		t.Error("child wire borrowed or advanced the active parent Turn")
	}
	if root := parent.Usage.LastTransport; root != (llm.TransportRequest{}) {
		initial := v.ModelAttempts[root.AttemptID]
		if root.ModelCallID != parentTurn.ID || root.Purpose != "agent" || root.TransportAttempt != 1 || initial.Scope.InvocationID != scope.ParentInvocationID || initial.Scope.TurnID != parentTurn.ID {
			t.Error("child wire replaced its observed parent's original request identity")
		}
	}
	stored, err := s.rt.opts.Store.Load(context.WithoutCancel(r.Context()), s.rt.opts.SessionID)
	if err != nil {
		t.Error("cannot inspect the committed JSONL request reservation")
		return
	}
	for i := len(stored.Commits) - 1; i >= 0; i-- {
		commit := stored.Commits[i]
		for _, record := range commit.ControlRecords {
			if record.Type != "invocation_budget" || record.ID != scope.InvocationID {
				continue
			}
			var saved state.InvocationBudget
			if json.Unmarshal(record.Payload, &saved) != nil || !reflect.DeepEqual(saved, budget) || len(commit.ControlRecords) != 2 || len(commit.Entries) != 0 || len(commit.BranchUpdates) != 0 || len(commit.Events) != 1 || commit.Events[0].Type != "model.invocation_budget_reserved" {
				t.Error("physical reservation was not the exact private joint budget transaction")
			}
			var pairedTrace, beforeTrace state.TraceState
			started := false
			for _, control := range commit.ControlRecords {
				if control.Type == "trace" && control.ID == scope.TraceID {
					if json.Unmarshal(control.Payload, &pairedTrace) != nil {
						t.Error("invalid paired parent trace reservation")
					}
				}
			}
			for _, prior := range stored.Commits[:i] {
				for _, control := range prior.ControlRecords {
					if control.Type == "trace" && control.ID == scope.TraceID {
						if json.Unmarshal(control.Payload, &beforeTrace) != nil {
							t.Error("invalid preceding parent trace")
						}
					}
					if control.Type == "model_attempt" && control.ID == attempt.ID {
						var initial state.ModelAttempt
						started = json.Unmarshal(control.Payload, &initial) == nil && reflect.DeepEqual(initial, attempt)
					}
				}
			}
			// The activity lease may renew between this commit and View().
			// Compare the complete parent against its actual preceding journal
			// record, with exactly one new physical slot and no other change.
			beforeTrace.Usage.TransportRequests++
			paired := pairedTrace.Usage == parent.Usage && reflect.DeepEqual(pairedTrace, beforeTrace)
			if !paired || !started {
				t.Errorf("child attempt and parent occupancy were not durable before wire: paired=%t started=%t", paired, started)
			}
			return
		}
	}
	t.Error("offline wire has no committed child reservation")
}

func p3AuxiliaryLimitTerminals(t *testing.T, s *AgentSession, v state.View, inv state.Invocation) {
	t.Helper()
	stored, err := s.rt.opts.Store.Load(t.Context(), s.rt.opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	terminals := map[string]int{}
	invocationTerminals, traceTerminals, todoCommits := 0, 0, 0
	for _, commit := range stored.Commits {
		for _, record := range commit.ControlRecords {
			switch record.Type {
			case "model_attempt_transition":
				var result state.ModelAttemptTransition
				if json.Unmarshal(record.Payload, &result) != nil {
					t.Fatal("invalid attempt terminal in the actual journal")
				}
				initial := v.ModelAttempts[result.AttemptID]
				if initial.Scope.InvocationID != inv.ID {
					continue
				}
				terminals[initial.ID]++
				if result.ExpectedRevision != 1 || result.Revision != 2 || initial.Scope.TurnID != "" || initial.Scope.SelectionRevision != 0 || initial.Scope.ParentInvocationID != inv.ParentInvocationID || len(commit.Entries) != 0 || len(commit.BranchUpdates) != 0 {
					t.Error("child terminal changed original identity or parent history")
				}
				if result.State != "accepted" {
					for _, control := range commit.ControlRecords {
						if control.Type == "tool_call" || control.Type == "invocation_message" {
							t.Error("budget-refused attempt accepted a candidate or tools")
						}
					}
				}
			case "todo_update":
				var update state.TodoUpdate
				if json.Unmarshal(record.Payload, &update) != nil || !reflect.DeepEqual(update, v.TodoUpdates[record.ID]) || update.InvocationID != inv.ID || len(commit.ControlRecords) != 1 || len(commit.Events) != 1 || commit.Events[0].Type != "tool.finished" {
					t.Error("confirmed tool effect lost its sole durable receipt transaction")
				}
				todoCommits++
			case "invocation":
				var saved state.Invocation
				if json.Unmarshal(record.Payload, &saved) == nil && saved.ID == inv.ID && terminal(saved.State) {
					invocationTerminals++
				}
			}
		}
		for _, event := range commit.Events {
			if event.Type == "trace.settled" && event.Scope.TraceID == inv.TraceID {
				traceTerminals++
			}
		}
	}
	for _, attempt := range childAttemptRecords(v, inv.ID) {
		if terminals[attempt.ID] != 1 || v.AttemptResults[attempt.ID].AttemptID != attempt.ID {
			t.Error("child attempt has no sole durable terminal")
		}
	}
	if invocationTerminals != 1 || traceTerminals != 1 {
		t.Errorf("terminal counts: invocation=%d trace=%d, want one each", invocationTerminals, traceTerminals)
	}
	if todoCommits != len(v.TodoUpdates) {
		t.Error("already confirmed effects were repeated or lost from the actual journal")
	}
	for _, turn := range v.Turns {
		if turn.InvocationID != v.Traces[inv.TraceID].InvocationID {
			t.Error("auxiliary attempt created or borrowed a parent Turn")
		}
	}
	for _, msg := range v.Messages {
		if msg.Scope.InvocationID == inv.ID || msg.Kind == agent.KindCompactionSummary {
			t.Error("private auxiliary material changed the parent projection")
		}
	}
}

// ProfileMemory permits controllable test models; the real state root still
// creates a JSONL store. Open explicitly requests public read-only browsing.
func p3AuxiliaryLimitReadOnlyReopen(t *testing.T, s *AgentSession, opts Options, before state.View) *AgentSession {
	t.Helper()
	if _, ok := s.rt.opts.Store.(*jsonl.Store); !ok {
		t.Fatal("the test requires the real JSONL Create/Close/Open path")
	}
	stored, err := s.rt.opts.Store.Load(t.Context(), opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(journalPath(opts.StateRoot, opts.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opts.ReadOnly = true
	s = openSubagentSession(t, opts, false)
	if _, err := s.Snapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := s.rt.manager.View()
	reopened, err := s.rt.opts.Store.Load(t.Context(), opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.Commits, reopened.Commits) || stored.LastSeq != reopened.LastSeq || !reflect.DeepEqual(before.InvocationBudgets, after.InvocationBudgets) || !reflect.DeepEqual(before.Invocations, after.Invocations) || !reflect.DeepEqual(before.ModelAttempts, after.ModelAttempts) || !reflect.DeepEqual(before.AttemptResults, after.AttemptResults) || !reflect.DeepEqual(before.AttemptDetails, after.AttemptDetails) || !reflect.DeepEqual(before.InvocationMessages, after.InvocationMessages) || !reflect.DeepEqual(before.Calls, after.Calls) || !reflect.DeepEqual(before.Todos, after.Todos) || !reflect.DeepEqual(before.TodoUpdates, after.TodoUpdates) || !reflect.DeepEqual(before.Traces, after.Traces) || !reflect.DeepEqual(before.Turns, after.Turns) {
		t.Error("read-only JSONL Open wrote state, refunded occupancy or changed terminal evidence")
	}
	readOnlyJournal, err := os.ReadFile(journalPath(opts.StateRoot, opts.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(journal, readOnlyJournal) {
		t.Error("Close/read-only Open changed the actual JSONL bytes")
	}
	return s
}

func p3AuxiliaryLimitConfirmedTodos(t *testing.T, v state.View, invocationID string) {
	t.Helper()
	calls := callsNamed(v, "write_todos")
	if len(calls) != 3 || len(v.TodoUpdates) != 3 || len(v.Todos) != 1 || v.Todos[invocationID].Version != 3 {
		t.Error("three real invocation-local TODO effects and receipts are required")
	}
	versions := map[uint64]bool{}
	for _, call := range calls {
		update := v.TodoUpdates[call.Call.CallID]
		frozen := v.FrozenExecutions["execution:"+call.Call.CallID]
		if call.Scope.InvocationID != invocationID || !call.Claimed || call.Observation == nil || !call.Observation.Executed || call.Observation.Status != "succeeded" || call.Observation.SideEffect != "confirmed" {
			t.Error("auxiliary refusal discarded an already confirmed controlled tool effect")
			continue
		}
		if update.CallID != call.Call.CallID || update.InvocationID != invocationID || update.Version < 1 || update.Version > 3 || versions[update.Version] || update.FrozenHash != frozen.Hash || !bytes.Equal(update.Content, frozen.FinalArguments) || string(update.Content) != call.Observation.Content {
			t.Error("TODO receipt changed its original call, frozen arguments or consecutive version")
		}
		versions[update.Version] = true
		if update.Version == 3 && !reflect.DeepEqual(update, v.Todos[invocationID]) {
			t.Error("auxiliary refusal replaced the latest confirmed TODO effect")
		}
	}
}

// Resolve the window on the existing observed helper and inspect the real
// auxiliary entrance; all reservations still use the session adapter.
type p3AuxiliaryLimitWindowModel struct {
	*p3ChildAttemptModel
	t         *testing.T
	summaries atomic.Int32
}

func (*p3AuxiliaryLimitWindowModel) EffectiveOptions() llm.EffectiveOptions {
	return llm.EffectiveOptions{ContextWindowTokens: 8192, MaxOutputTokens: 16}
}

func (m *p3AuxiliaryLimitWindowModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	scope := einorun.ScopeFromContext(ctx, agent.ExecutionScope{})
	v := m.manager.View()
	for _, attempt := range childAttemptRecords(v, scope.InvocationID) {
		if _, ended := v.AttemptResults[attempt.ID]; !ended && attempt.Purpose == "compaction" {
			m.summaries.Add(1)
			// This is the real auxiliary Generate entrance, after three real
			// controlled writes, before its first physical reservation is denied.
			p3AuxiliaryLimitConfirmedTodos(m.t, v, scope.InvocationID)
			break
		}
	}
	return m.p3ChildAttemptModel.Generate(ctx, in, opts...)
}

func TestP3ChildAuxiliaryLimitCompactionTracePhysical(t *testing.T) {
	steps := []testkit.Step{}
	for i := 1; i <= 3; i++ {
		title := "done-" + strconv.Itoa(i)
		if i == 3 {
			// Arguments and the real TODO result together provide 30KB of
			// committed material: over soft threshold, but still hard-fit.
			title = strings.Repeat("o", 15000)
		}
		args := `{"items":[{"title":"` + title + `","state":"completed"}]}`
		steps = append(steps, testkit.Step{Text: "child step " + strconv.Itoa(i), ToolCalls: []schema.FunctionToolCall{{CallID: "todo-" + strconv.Itoa(i), Name: "write_todos", Arguments: args}}})
	}
	steps = append(steps, testkit.Step{Text: summaryText(), ToolCalls: []schema.FunctionToolCall{{CallID: "forbidden-summary-tool", Name: "write_todos", Arguments: `{"items":[]}`}}}, testkit.Step{Text: "child must not finish"})
	child := &p3AuxiliaryLimitWindowModel{p3ChildAttemptModel: newP3ChildAttemptModel(steps...), t: t}
	main := testkit.NewFake(delegateCall("worker", "own task for soft compaction"), testkit.Step{Text: "parent must not continue"})
	opts := subagentOptions(agentRoots(t), "auxiliary-compaction-physical", main, []agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"write_todos"}}}, []tools.Definition{builtinDefinitionForSession(t, "write_todos")})
	// Three accepted child responses consume six real physical requests; the
	// parent's initial request fills the shared seventh and last slot.
	opts.Limits = config.Limits{LogicalModelRequests: 2, TraceTransportRequests: 7}
	s := openSubagentSession(t, opts, true)
	child.manager = s.rt.manager
	child.beforeWire = func(r *http.Request) {
		n := int(child.physical.Load()) + 1
		purpose := "cache_query"
		if n%2 == 0 {
			purpose = "agent"
		}
		p3AuxiliaryLimitWireReservation(t, s, r, (n+1)/2, n, (n-1)%2+1, purpose)
	}
	in := submitPrompt(t, s, "delegate with a cumulative physical limit")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	v := s.rt.manager.View()
	inv := onlyInvocation(t, v)
	attempts := childAttemptRecords(v, inv.ID)
	budget, trace := v.InvocationBudgets[inv.ID], v.Traces[in.TraceID]
	if len(v.TodoUpdates) != 3 || child.summaries.Load() != 1 || child.generates.Load() != 5 || child.registered.Load() != 5 || child.physical.Load() != 6 || child.FakeModel.Calls() != 3 || main.Calls() != 1 || inv.State != "failed" || inv.ModelCalls != 5 || len(attempts) != 5 || len(inv.MessageIDs) != 3 || len(inv.CallIDs) != 3 || len(v.Calls) != 4 || budget.Usage.LogicalModelCalls != 5 || budget.Usage.TransportRequests != 6 || budget.Usage.ToolExecutions != 0 || len(budget.Calls) != 5 || trace.State != "failed" || trace.Usage.LogicalModelCalls != 7 || trace.Usage.TransportRequests != 7 || trace.Usage.ToolExecutions != 4 || len(v.Turns) != 2 {
		t.Fatalf("compaction physical limit: Generate=%d summary=%d registered=%d wire=%d candidate=%d effects=%d parent=%d attempts=%d invocation=%s child=%+v trace=%+v", child.generates.Load(), child.summaries.Load(), child.registered.Load(), child.physical.Load(), child.FakeModel.Calls(), len(v.TodoUpdates), main.Calls(), len(attempts), inv.State, budget.Usage, trace.Usage)
	}
	summaries, accepted, refusedAgent := 0, 0, 0
	for _, attempt := range attempts {
		result := v.AttemptResults[attempt.ID]
		details := v.AttemptDetails[result.DiagnosticRef]
		progress := budget.Calls[attempt.ModelCallID]
		switch {
		case attempt.Purpose == "agent" && result.State == "accepted":
			accepted++
			if progress.Requests != 2 || len(details.Usage) != 2 {
				t.Error("completed child work lost its physical reservations")
			}
		case attempt.Purpose == "compaction":
			summaries++
			if result.State != "failed" || details.FailureCode != product.CodeBudgetExhausted || progress.Requests != 0 || len(details.Usage) != 0 || v.InvocationMessages[attempt.MessageID].ID != "" {
				t.Error("shared physical limit admitted a summary candidate or refunded it")
			}
		case attempt.Purpose == "agent" && result.State == "failed":
			refusedAgent++
			if details.FailureCode != product.CodeBudgetExhausted || progress.Requests != 0 || len(details.Usage) != 0 || attempt.ModelCallID == budget.Usage.ModelCallID {
				t.Error("failed summary gave the original agent a new request slot")
			}
		default:
			t.Error("unexpected child attempt after shared physical refusal")
		}
	}
	if summaries != 1 || accepted != 3 || refusedAgent != 1 || budget.Usage.ModelRequests != 0 || budget.Usage.LastTransport != (llm.TransportRequest{}) {
		t.Fatal("real soft-threshold summary admission or original-call occupancy is missing")
	}
	p3AuxiliaryLimitConfirmedTodos(t, v, inv.ID)
	if out := delegateOutcome(t, delegateRecord(t, s)); out.Status != "failed" || out.Code != product.CodeBudgetExhausted || out.Result != "" {
		t.Fatalf("physical-limited delegate projection: %+v", out)
	}
	p3AuxiliaryLimitTerminals(t, s, v, inv)
	s = p3AuxiliaryLimitReadOnlyReopen(t, s, opts, v)
	if child.generates.Load() != 5 || child.summaries.Load() != 1 || child.physical.Load() != 6 || child.FakeModel.Calls() != 3 || len(s.rt.manager.View().TodoUpdates) != 3 || main.Calls() != 1 {
		t.Fatal("read-only Open repeated a refused summary or confirmed effects")
	}
	t.Log("Generate=5 (three agents, one refused summary, one refused original continuation), wire=6, excess wire=0, effects=3; child=5/6, parent=7/7; both refused calls keep requests=0; delegate=budget_exhausted; read-only JSONL replay verified")
}

func TestP3ChildAuxiliaryLimitCompactionTraceLogicalAfterOverflow(t *testing.T) {
	var requests atomic.Int32
	var s *AgentSession
	child := p2FactoryModel(t, func(r *http.Request) (*http.Response, error) {
		n := int(requests.Add(1))
		p3AuxiliaryLimitWireReservation(t, s, r, n, n, 1, "agent")
		switch n {
		case 1, 2, 3:
			return p3FactoryResponse(r, 200, p3FactoryAssistant("child step", &schema.FunctionToolCall{CallID: "todo-" + strconv.Itoa(n), Name: "write_todos", Arguments: `{"items":[{"title":"done","state":"completed"}]}`})), nil
		case 4:
			p3AuxiliaryLimitConfirmedTodos(t, s.rt.manager.View(), einorun.ScopeFromContext(r.Context(), agent.ExecutionScope{}).InvocationID)
			return p3FactoryResponse(r, 400, `{"error":{"code":"context_length_exceeded","message":"synthetic overflow"}}`), nil
		default:
			t.Error("logical-refused summary or original retry reached offline wire")
			return nil, product.NewError(product.CodeInternal, "unexpected test request")
		}
	}, func(cfg *llm.ModelConfig) {
		cfg.Capabilities.Items[llm.CapContextOverflow] = llm.Capability{Status: llm.Verified, AdapterVersion: "offline-child-fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline context_length_exceeded fixture"}}
	})
	main := testkit.NewFake(delegateCall("worker", "own task"), testkit.Step{Text: "parent must not continue"})
	opts := subagentOptions(agentRoots(t), "auxiliary-compaction-logical", main, []agent.AgentDefinition{{Name: "worker", Version: "w1", Instruction: "W", Model: child, Delegable: true, Tools: []string{"write_todos"}}}, []tools.Definition{builtinDefinitionForSession(t, "write_todos")})
	// There is physical and per-call retry room after the overflow. Only the
	// shared logical total refuses the fresh summary at BeginTurn, before an
	// auxiliary attempt exists. The compactor keeps the original overflow.
	opts.Limits = config.Limits{LogicalModelRequests: 2, TraceLogicalModelCalls: 5}
	s = openSubagentSession(t, opts, true)
	in := submitPrompt(t, s, "delegate with no logical slot for overflow summary")
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	v := s.rt.manager.View()
	inv := onlyInvocation(t, v)
	attempts := childAttemptRecords(v, inv.ID)
	budget, trace := v.InvocationBudgets[inv.ID], v.Traces[in.TraceID]
	if requests.Load() != 4 || len(v.TodoUpdates) != 3 || main.Calls() != 1 || inv.State != "failed" || inv.ModelCalls != 4 || len(attempts) != 4 || len(inv.MessageIDs) != 3 || len(inv.CallIDs) != 3 || len(v.Calls) != 4 || budget.Usage.LogicalModelCalls != 4 || budget.Usage.TransportRequests != 4 || budget.Usage.ModelRequests != 1 || len(budget.Calls) != 4 || trace.State != "failed" || trace.Usage.LogicalModelCalls != 5 || trace.Usage.TransportRequests != 5 || trace.Usage.ToolExecutions != 4 || len(v.Turns) != 1 {
		t.Fatalf("compaction logical limit: wire=%d effects=%d parent=%d attempts=%d invocation=%s child=%+v trace=%+v", requests.Load(), len(v.TodoUpdates), main.Calls(), len(attempts), inv.State, budget.Usage, trace.Usage)
	}
	accepted, overflow := 0, 0
	for _, attempt := range attempts {
		result := v.AttemptResults[attempt.ID]
		details := v.AttemptDetails[result.DiagnosticRef]
		if attempt.Purpose != "agent" || budget.Calls[attempt.ModelCallID].Requests != 1 || attempt.Attempt != 1 || len(details.Usage) != 1 {
			t.Error("refused summary added an auxiliary attempt or changed agent request slots")
		}
		if result.State == "accepted" {
			accepted++
		} else if result.State == "failed" && details.FailureCode == product.CodeResourceUnavailable && details.FailureReason == "context_overflow" && attempt.ModelCallID == budget.Usage.ModelCallID {
			overflow++
		} else {
			t.Error("summary refusal replaced the original certified overflow terminal")
		}
	}
	if accepted != 3 || overflow != 1 {
		t.Fatal("three accepted responses and the sole original overflow are required")
	}
	p3AuxiliaryLimitConfirmedTodos(t, v, inv.ID)
	// The summary's budget error is swallowed by childCompactor. The delegate
	// reports the original resource_unavailable/no replacement projection,
	// rather than claiming that the auxiliary budget code propagated to it.
	if out := delegateOutcome(t, delegateRecord(t, s)); out.Status != "failed" || out.Code != product.CodeResourceUnavailable || out.InvocationID != inv.ID || out.Result != "" {
		t.Fatalf("original overflow delegate projection: %+v", out)
	}
	p3AuxiliaryLimitTerminals(t, s, v, inv)
	s = p3AuxiliaryLimitReadOnlyReopen(t, s, opts, v)
	if requests.Load() != 4 || len(s.rt.manager.View().TodoUpdates) != 3 || main.Calls() != 1 {
		t.Fatal("read-only Open retried overflow, summary or confirmed tools")
	}
	t.Log("agent wire/attempts=4/4, summary wire/attempts=0/0 (logical admission refused), retry wire=0, effects=3; child=4/4, parent=5/5; original requests=1 remains; delegate=resource_unavailable/context_overflow, not the swallowed summary budget code; read-only JSONL replay verified")
}
