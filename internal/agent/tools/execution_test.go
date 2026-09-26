package tools

import (
	"context"
	"encoding/json"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
)

type memSink struct{ facts int }

func (s *memSink) CommitFact(context.Context, agent.ExecutionScope, agent.Fact) error {
	s.facts++
	return nil
}

func TestInvalidArgumentsDoNotRun(t *testing.T) {
	var calls int
	sink := &memSink{}
	exec, err := NewExecutor("gen", []Definition{{
		Name: "add", Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
		Run: func(context.Context, json.RawMessage) (string, error) { calls++; return "1", nil },
	}}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(context.Background(), agent.ExecutionScope{}, "c1", "add", `{"n":"nope"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.Executed || calls != 0 || sink.facts != 0 {
		t.Fatalf("out=%+v calls=%d facts=%d", out, calls, sink.facts)
	}
}

type allow struct{}

func (allow) Authorize(context.Context, agent.FrozenCall) (agent.Decision, error) {
	return agent.DecisionAllow, nil
}

type deny struct{}

func (deny) Authorize(context.Context, agent.FrozenCall) (agent.Decision, error) {
	return agent.DecisionDeny, nil
}

type recordSink struct {
	facts        []agent.Fact
	rec          agent.ToolRecord
	found        bool
	at           []int
	budg         *agent.BudgetLedger
	scopes       []agent.ExecutionScope
	beforeCommit func(agent.Fact) error
	afterCommit  func(agent.Fact)
	validator    agent.ExecutionTicketValidator
}

func (s *recordSink) CommitFact(_ context.Context, scope agent.ExecutionScope, fact agent.Fact) error {
	if s.beforeCommit != nil {
		if err := s.beforeCommit(fact); err != nil {
			return err
		}
	}
	if fact.Kind == "tool_intent" && fact.Budget != nil {
		// Inspect the candidate, never re-enter the ledger while it commits.
		s.at = append(s.at, fact.Budget.ToolExecutions)
	}
	s.scopes = append(s.scopes, scope)
	s.facts = append(s.facts, fact)
	if s.afterCommit != nil {
		s.afterCommit(fact)
	}
	return nil
}

func (s *recordSink) ValidateExecutionTicket(ctx context.Context, frozen agent.FrozenExecution) error {
	if s.validator != nil {
		return s.validator.ValidateExecutionTicket(ctx, frozen)
	}
	return nil
}

func (s *recordSink) LookupTool(context.Context, agent.ExecutionScope, string) (agent.ToolRecord, error) {
	if !s.found {
		return agent.ToolRecord{}, product.NewError(product.CodeNotFound, "not accepted")
	}
	return s.rec, nil
}

func addDef(run func(context.Context, json.RawMessage) (string, error)) Definition {
	return Definition{
		Version: "1", Name: "add",
		Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
		Run:    run,
	}
}

func accepted(args string) agent.ToolRecord {
	return agent.ToolRecord{Call: agent.FrozenCall{CallID: "prod-1", ProviderCallID: "prov-1", Name: "add", Arguments: args, Generation: "gen"}}
}

func TestAcceptedSchemaDenyAndBudgetPersistObservation(t *testing.T) {
	var calls int
	limits := config.DefaultLimits()
	limits.TraceToolCalls = 0
	// WithDefaults replaces 0, so force exhaustion by occupying the only slot after a tiny limit.
	limits.TraceToolCalls = 1
	budg := agent.NewBudget(limits)
	if err := budg.OccupyTool(); err != nil {
		t.Fatal(err)
	}
	sink := &recordSink{found: true, budg: budg, rec: accepted(`{"n":1}`)}
	exec, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "1", nil
	})}, sink, allow{}, budg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(context.Background(), agent.ExecutionScope{TurnID: "t"}, "prov-1", "add", `{"n":1}`)
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeBudgetExhausted {
		t.Fatalf("err=%v", err)
	}
	if out.Executed || calls != 0 || !hasObservation(t, sink, "failed") {
		t.Fatalf("out=%+v calls=%d facts=%d", out, calls, len(sink.facts))
	}

	sink = &recordSink{found: true, rec: accepted(`{"n":"nope"}`)}
	exec, err = NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "1", nil
	})}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Run(context.Background(), agent.ExecutionScope{}, "prov-1", "add", `{"n":"nope"}`); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || !hasObservation(t, sink, "failed") {
		t.Fatalf("schema failure facts=%d calls=%d", len(sink.facts), calls)
	}

	sink = &recordSink{found: true, rec: accepted(`{"n":1}`)}
	exec, err = NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "1", nil
	})}, sink, deny{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Run(context.Background(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || !hasObservation(t, sink, "denied") {
		t.Fatalf("deny facts=%d", len(sink.facts))
	}
}

func TestClaimedWithoutObservationDoesNotRerun(t *testing.T) {
	var calls int
	rec := accepted(`{"n":1}`)
	rec.Claimed = true
	sink := &recordSink{found: true, rec: rec}
	exec, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "1", nil
	})}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = exec.Run(context.Background(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
	if err == nil || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestExistingObservationIsReused(t *testing.T) {
	var calls int
	rec := accepted(`{"n":1}`)
	rec.Observation = &agent.ToolObservation{Status: "succeeded", Content: "cached", SideEffect: "none", Executed: true}
	sink := &recordSink{found: true, rec: rec}
	exec, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "fresh", nil
	})}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(context.Background(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || out.Content != "cached" || len(sink.facts) != 0 {
		t.Fatalf("out=%+v calls=%d facts=%d", out, calls, len(sink.facts))
	}
}

func TestAcceptedNameOrArgumentsMustMatch(t *testing.T) {
	var calls int
	sink := &recordSink{found: true, rec: accepted(`{"n":2}`)}
	exec, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "1", nil
	})}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(context.Background(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || out.Status == "succeeded" {
		t.Fatalf("out=%+v calls=%d", out, calls)
	}
}

func TestP2BudgetIntentAndOccupancyAreOneFact(t *testing.T) {
	budg := agent.NewBudget(config.DefaultLimits())
	sink := &recordSink{found: true, budg: budg, rec: accepted(`{"n":1}`)}
	exec, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		return "1", nil
	})}, sink, allow{}, budg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Run(context.Background(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`); err != nil {
		t.Fatal(err)
	}
	if len(sink.at) != 1 || sink.at[0] != 1 {
		t.Fatalf("tool executions at intent time = %v", sink.at)
	}
	var intent agent.FrozenCall
	if err := json.Unmarshal(sink.facts[0].Payload, &intent); err != nil || intent.CallID != "prod-1" || intent.ProviderCallID != "prov-1" {
		t.Fatalf("intent fact=%s err=%v", sink.facts[0].Payload, err)
	}
}

func TestNilRunAndPanicDoNotRerun(t *testing.T) {
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	exec, err := NewExecutor("gen", []Definition{addDef(nil)}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(context.Background(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
	if err != nil || out.Executed || out.Status == "succeeded" {
		t.Fatalf("nil run out=%+v err=%v", out, err)
	}

	var calls int
	sink = &recordSink{found: true, rec: accepted(`{"n":1}`)}
	exec, err = NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		calls++
		panic("boom")
	})}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	out, err = exec.Run(context.Background(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
	if err != nil || calls != 1 || out.Status != "failed" || !out.Executed {
		t.Fatalf("panic out=%+v err=%v calls=%d", out, err, calls)
	}
}

func TestToolRunUsesTimeoutContext(t *testing.T) {
	limits := config.DefaultLimits()
	limits.ToolTimeout = 20 * time.Millisecond
	budg := agent.NewBudget(limits)
	sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
	exec, err := NewExecutor("gen", []Definition{addDef(func(ctx context.Context, _ json.RawMessage) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})}, sink, allow{}, budg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var out Outcome
	var runErr error
	go func() {
		out, runErr = exec.Run(context.Background(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tool run ignored the timeout context")
	}
	if runErr != nil || out.Status == "succeeded" {
		t.Fatalf("out=%+v err=%v", out, runErr)
	}
}

func hasObservation(t *testing.T, sink *recordSink, status string) bool {
	t.Helper()
	for _, fact := range sink.facts {
		if fact.Kind != "tool_observation" {
			continue
		}
		var rec agent.ToolRecord
		if json.Unmarshal(fact.Payload, &rec) != nil || rec.Observation == nil {
			continue
		}
		if rec.Observation.Status == status {
			return true
		}
	}
	return false
}

func TestBudgetFailureDoesNotClaimAndReturnsError(t *testing.T) {
	limits := config.DefaultLimits()
	limits.TraceToolCalls = 1
	budg := agent.NewBudget(limits)
	if err := budg.OccupyTool(); err != nil {
		t.Fatal(err)
	}
	rec := accepted(`{"n":1}`)
	rec.Call.Hash = "parent-hash"
	rec.Scope = agent.ExecutionScope{TraceID: "tr", TurnID: "turn-real"}
	sink := &recordSink{found: true, rec: rec}
	exec, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		t.Fatal("budget failure reran the tool")
		return "", nil
	})}, sink, allow{}, budg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(context.Background(), agent.ExecutionScope{TraceID: "tr"}, "prov-1", "add", `{"n":1}`)
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeBudgetExhausted {
		t.Fatalf("err=%v out=%+v", err, out)
	}
	if out.Executed || out.SideEffect != "none" {
		t.Fatalf("out=%+v", out)
	}
	var saw bool
	for i, fact := range sink.facts {
		if fact.Kind != "tool_observation" {
			continue
		}
		saw = true
		var saved agent.ToolRecord
		if err := json.Unmarshal(fact.Payload, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.Claimed || saved.Observation == nil || saved.Observation.Executed || saved.Observation.SideEffect != "none" {
			t.Fatalf("observation %+v", saved)
		}
		if saved.Call.Hash != "parent-hash" {
			t.Fatalf("hash rewritten to %s", saved.Call.Hash)
		}
		if sink.scopes[i].TurnID != "turn-real" {
			t.Fatalf("scope %+v", sink.scopes[i])
		}
	}
	if !saw {
		t.Fatal("budget failure did not persist an observation")
	}
}

func TestAcceptedFrozenHashAndScopeArePreserved(t *testing.T) {
	rec := accepted(`{"n":1}`)
	rec.Call.Hash = "parent-hash"
	rec.Scope = agent.ExecutionScope{TraceID: "tr", TurnID: "turn-real"}
	sink := &recordSink{found: true, rec: rec}
	exec, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		return "1", nil
	})}, sink, allow{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Run(context.Background(), agent.ExecutionScope{TraceID: "missing-turn"}, "prov-1", "add", `{"n":1}`); err != nil {
		t.Fatal(err)
	}
	if len(sink.facts) == 0 || len(sink.scopes) != len(sink.facts) {
		t.Fatalf("facts=%d scopes=%d", len(sink.facts), len(sink.scopes))
	}
	for i, fact := range sink.facts {
		if sink.scopes[i].TurnID != "turn-real" {
			t.Fatalf("fact %s scope %+v", fact.Kind, sink.scopes[i])
		}
		var call agent.FrozenCall
		payload := fact.Payload
		if fact.Kind == "tool_observation" {
			var saved agent.ToolRecord
			if err := json.Unmarshal(payload, &saved); err != nil {
				t.Fatal(err)
			}
			call = saved.Call
		} else if err := json.Unmarshal(payload, &call); err != nil {
			t.Fatal(err)
		}
		if call.Hash != "parent-hash" {
			t.Fatalf("%s hash=%s", fact.Kind, call.Hash)
		}
	}
}
