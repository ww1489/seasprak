package state_test

import (
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/llm"
)

func TestP3DelegatedTraceBudgetPreservesObservedParentTurn(t *testing.T) {
	m, _, call := budgetCall(t)
	ledger := agent.NewBudget(config.Limits{})
	ledger.SetPersist(func(u agent.Usage) error { return m.SaveTraceBudget(t.Context(), call.Scope.TraceID, u) })
	if err := ledger.BeginTurnID(call.Scope.TurnID); err != nil {
		t.Fatal(err)
	}
	request := llm.TransportRequest{RequestIdentity: llm.RequestIdentity{ModelCallID: call.Scope.TurnID, AttemptID: "observed-parent-attempt", Purpose: "agent"}, TransportAttempt: 1}
	if err := ledger.BeforeRequest(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	before := m.View()
	if err := ledger.ChargeDelegated(1, 0); err != nil {
		t.Fatal("independent summary logical reservation was rejected", err)
	}
	if err := ledger.ChargeDelegated(0, 1); err != nil {
		t.Fatal("independent summary request was mistaken for a new parent request", err)
	}
	after := m.View()
	usage := after.Traces[call.Scope.TraceID].Usage
	if usage != ledger.Snapshot() || usage.LogicalModelCalls != 2 || usage.TransportRequests != 2 || usage.ModelCallID != before.Traces[call.Scope.TraceID].Usage.ModelCallID || usage.ModelRequests != 1 || usage.LastTransport != request || !reflect.DeepEqual(before.Turns, after.Turns) {
		t.Fatal("delegated aggregate occupancy changed parent request progress")
	}
	for _, event := range after.Events[len(before.Events):] {
		if event.Type == "model.transport_reserved" || event.Type == "turn_start" {
			t.Fatal("independent summary was represented as another parent request or Turn")
		}
	}
}
