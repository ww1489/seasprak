package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
)

func TestStandaloneUnknownHoldsHaveDistinctOwners(t *testing.T) {
	scheduler := NewResourceScheduler()
	for _, workspace := range []string{"workspace-a", "workspace-b"} {
		sink := &recordSink{found: true, rec: accepted(`{"n":1}`)}
		exec, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
			panic("synthetic unknown effect")
		})}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithResourceScheduler(scheduler), WithResourceDomain("memory", workspace))
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
		if err != nil || !out.Executed || out.SideEffect != "unknown" {
			t.Fatalf("unknown call result=%+v err=%v", out, err)
		}
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if len(scheduler.holds) != 2 {
		t.Fatalf("unrelated standalone calls collided on one hold identity: holds=%d", len(scheduler.holds))
	}
	for key, use := range scheduler.active {
		if use.transientReaders != 0 || use.transientWriters != 0 {
			t.Fatalf("unknown call lost its durable hold and leaked a transient lease: key=%q use=%+v", key, use)
		}
	}
}

func TestStandaloneDomainSurvivesExecutorAddressReuse(t *testing.T) {
	scheduler := NewResourceScheduler()
	first, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		panic("synthetic unknown effect")
	})}, &recordSink{found: true, rec: accepted(`{"n":1}`)}, allow{}, agent.NewBudget(config.DefaultLimits()), WithResourceScheduler(scheduler))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := first.Run(t.Context(), agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`); err != nil || out.SideEffect != "unknown" {
		t.Fatalf("first unknown result=%+v err=%v", out, err)
	}
	var calls int
	next, err := NewExecutor("gen", []Definition{addDef(func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "independent result", nil
	})}, &recordSink{found: true, rec: accepted(`{"n":1}`)}, allow{}, agent.NewBudget(config.DefaultLimits()), WithResourceScheduler(scheduler))
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the old object's address deterministically, as allocator reuse can
	// after the prior executor is collected. Its unresolved hold must remain.
	*first = *next
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	out, err := first.Run(ctx, agent.ExecutionScope{}, "prov-1", "add", `{"n":1}`)
	if err != nil || out.Status != "succeeded" || calls != 1 || out.Content != "independent result" {
		t.Fatalf("new executor reused an old unresolved scheduling domain: out=%+v calls=%d err=%v", out, calls, err)
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if len(scheduler.holds) != 1 {
		t.Fatalf("independent execution released prior unknown hold: %d", len(scheduler.holds))
	}
}
