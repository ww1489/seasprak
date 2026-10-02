package agent

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/llm"
)

func TestP3DelegatedBudgetJointCommitAndFailure(t *testing.T) {
	b := NewBudget(config.Limits{TraceLogicalModelCalls: 5, TraceTransportRequests: 10})
	original := Usage{LogicalModelCalls: 3, TransportRequests: 7, ToolExecutions: 1, ModelCallID: "parent", ModelRequests: 2,
		LastTransport: llm.TransportRequest{RequestIdentity: llm.RequestIdentity{ModelCallID: "parent", AttemptID: "attempt", Purpose: "agent"}, TransportAttempt: 2}}
	b.Restore(original)
	var ordinary, joint int
	b.SetPersist(func(Usage) error { ordinary++; return nil })
	want := original
	want.LogicalModelCalls++
	want.TransportRequests++
	sentinel := errors.New("synthetic joint append failure")
	if err := b.ChargeDelegatedWith(1, 1, func(u Usage) error {
		joint++
		if u != want {
			t.Fatal("joint commit changed parent progress or omitted occupancy")
		}
		return sentinel
	}); !errors.Is(err, sentinel) || b.Snapshot() != original || joint != 1 || ordinary != 0 {
		t.Fatal("failed joint commit consumed occupancy or called the ordinary persister")
	}
	if err := b.ChargeDelegatedWith(1, 1, func(u Usage) error {
		joint++
		if u != want {
			t.Fatal("joint commit candidate changed after rejection")
		}
		return nil
	}); err != nil || b.Snapshot() != want || joint != 2 || ordinary != 0 {
		t.Fatal("joint commit did not atomically swap the parent candidate")
	}
	for _, delta := range [][2]int{{-1, 0}, {0, -1}, {2, 0}, {0, 3}, {math.MaxInt, math.MaxInt}} {
		if err := b.ChargeDelegatedWith(delta[0], delta[1], func(Usage) error { t.Fatal("invalid charge persisted"); return nil }); err == nil || b.Snapshot() != want {
			t.Fatal("invalid delegated occupancy changed the parent ledger")
		}
	}
	if err := b.ChargeDelegatedWith(0, 1, nil); err == nil || b.Snapshot() != want {
		t.Fatal("joint occupancy accepted a missing persister")
	}
}

func TestP3DelegatedBudgetSerializesConcurrentChildren(t *testing.T) {
	b := NewBudget(config.Limits{TraceLogicalModelCalls: 3, TraceTransportRequests: 3})
	var persisted atomic.Int32
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			if b.ChargeDelegatedWith(1, 1, func(Usage) error { persisted.Add(1); return nil }) == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if persisted.Load() != 3 || accepted.Load() != 3 || b.Snapshot() != (Usage{LogicalModelCalls: 3, TransportRequests: 3}) {
		t.Fatal("concurrent children overbooked or lost a committed charge")
	}
}

func TestP3DelegatedBudgetPreservesParentRequestIdentity(t *testing.T) {
	b := NewBudget(config.DefaultLimits())
	if err := b.BeginTurnID("parent-call"); err != nil {
		t.Fatal(err)
	}
	request := llm.TransportRequest{RequestIdentity: llm.RequestIdentity{ModelCallID: "parent-call", AttemptID: "parent-attempt", Purpose: "agent"}, TransportAttempt: 1}
	if err := b.BeforeRequest(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	before := b.Snapshot()
	if err := b.ChargeDelegated(1, 2); err != nil {
		t.Fatal(err)
	}
	after := b.Snapshot()
	want := before
	want.LogicalModelCalls++
	want.TransportRequests += 2
	if after != want {
		t.Fatalf("child occupancy changed the parent's request identity or per-call progress: got %+v want %+v", after, want)
	}
}
