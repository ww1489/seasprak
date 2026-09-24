package agent

import (
	"errors"
	"github.com/ww1489/seasprak/internal/config"
	"testing"
)

func TestBeginTurnCountsLogicalAndOccupyCountsTransport(t *testing.T) {
	limits := config.Limits{LogicalModelRequests: 2, TraceLogicalModelCalls: 2, TraceTransportRequests: 5, TraceToolCalls: 3}
	b := NewBudget(limits)
	var persisted []Usage
	b.SetPersist(func(u Usage) error {
		persisted = append(persisted, u)
		return nil
	})
	if err := b.BeginTurn(); err != nil {
		t.Fatal(err)
	}
	if err := b.OccupyModel(); err != nil {
		t.Fatal(err)
	}
	if err := b.OccupyModel(); err != nil {
		t.Fatal(err)
	}
	if err := b.OccupyModel(); err == nil {
		t.Fatal("per-turn logical request limit was not enforced")
	}
	snap := b.Snapshot()
	if snap.LogicalModelCalls != 1 || snap.TransportRequests != 2 {
		t.Fatalf("usage = %+v, want logical 1 transport 2", snap)
	}
	if err := b.BeginTurn(); err != nil {
		t.Fatal(err)
	}
	if err := b.OccupyModel(); err != nil {
		t.Fatal(err)
	}
	if got := b.Snapshot(); got.LogicalModelCalls != 2 || got.TransportRequests != 3 {
		t.Fatalf("after second turn usage = %+v", got)
	}
	if err := b.BeginTurn(); err == nil {
		t.Fatal("trace logical limit was not enforced")
	}
	if b.Snapshot().LogicalModelCalls != 2 {
		t.Fatal("rejected BeginTurn changed usage")
	}
	if len(persisted) == 0 || persisted[len(persisted)-1] != b.Snapshot() {
		t.Fatalf("persist did not observe the committed usage, last=%v snap=%+v", persisted, b.Snapshot())
	}
	if b.Limits().LogicalModelRequests != 2 || b.Limits().TraceToolCalls != 3 {
		t.Fatalf("limits = %+v", b.Limits())
	}
}

func TestPersistFailureDoesNotSwapUsage(t *testing.T) {
	b := NewBudget(config.Limits{TraceLogicalModelCalls: 4, TraceTransportRequests: 4, TraceToolCalls: 2})
	b.SetPersist(func(Usage) error { return errors.New("disk") })
	if err := b.BeginTurn(); err == nil || b.Snapshot().LogicalModelCalls != 0 {
		t.Fatalf("err=%v usage=%+v", err, b.Snapshot())
	}
	if err := b.OccupyModel(); err == nil || b.Snapshot().TransportRequests != 0 {
		t.Fatalf("err=%v usage=%+v", err, b.Snapshot())
	}
	if err := b.OccupyTool(); err == nil || b.Snapshot().ToolExecutions != 0 {
		t.Fatalf("err=%v usage=%+v", err, b.Snapshot())
	}
}

func TestRestoreReplacesUsage(t *testing.T) {
	b := NewBudget(config.DefaultLimits())
	want := Usage{LogicalModelCalls: 4, TransportRequests: 9, ToolExecutions: 3}
	b.Restore(want)
	if b.Snapshot() != want {
		t.Fatalf("restored %+v", b.Snapshot())
	}
}
