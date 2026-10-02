package eino

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

func p3ProbeClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func p3CleanupJoin(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(8 * time.Second):
		t.Errorf("timeout joining %s", what)
	}
}

// A noncooperative in-process descendant must leave cancellation pending until
// its actual code exits. Delivering cancellation is not a stopped proof.
func TestP3ChildResumeContractNoncooperativeToolRequiresExit(t *testing.T) {
	p := newP3ChildProbe()
	p.block = &p3ChildBlock{entered: make(chan struct{}), gate: make(chan struct{}), exited: make(chan struct{}), cancelObserved: make(chan struct{}), ignoreCancellation: true}
	p.bEntered, p.doneEntered = make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx = WithExecutionScope(ctx, p.scope)
	ag, err := p.newAgent(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	store := &p3WorkflowStore{}
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag, CheckPointStore: store})
	done := make(chan struct{})
	type consumerResult struct {
		err        error
		toolExited bool
	}
	result := make(chan consumerResult, 1)
	go func() {
		_, runErr := p3CollectChildProbe(runner.Run(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("task:root")}, adk.WithCheckPointID("tree")))
		result <- consumerResult{err: runErr, toolExited: p3ProbeClosed(p.block.exited)}
		close(done)
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(p.block.gate) }) }
	t.Cleanup(func() {
		cancel()
		release()
		p3CleanupJoin(t, p.block.exited, "noncooperative descendant cleanup")
		p3CleanupJoin(t, done, "root runner cleanup")
	})
	p2Await(t, p.block.entered, "noncooperative nested tool entry")
	p2Await(t, p.bEntered, "sibling approval entry")
	p2Await(t, p.doneEntered, "completed sibling entry")
	before := p.counts()
	for _, m := range p.models {
		if m.Calls() != 1 {
			t.Fatal("cancellation snapshot preceded a sibling model entry")
		}
	}
	cancel()
	p2Await(t, p.block.cancelObserved, "cancellation delivered to nested tool")
	if p3ProbeClosed(done) || p3ProbeClosed(p.block.exited) {
		t.Fatal("noncooperative probe gate did not keep actual descendant code running")
	}
	if _, ok, _ := store.Get(context.Background(), "tree"); ok {
		t.Fatal("unjoined cancelled execution advertised a checkpoint")
	}
	release()
	p2Await(t, p.block.exited, "actual noncooperative tool exit")
	p2Await(t, done, "native root runner exit")
	out := <-result
	if !out.toolExited {
		t.Fatal("native event consumer returned before the actual descendant exit")
	}
	if !errors.Is(out.err, context.Canceled) {
		t.Fatal("cancelled native runner lost cancellation identity")
	}
	if _, ok, _ := store.Get(context.Background(), "tree"); ok {
		t.Fatal("cancelled runner saved a resumable checkpoint after exit")
	}
	if !reflect.DeepEqual(p.counts(), before) {
		t.Fatalf("cancellation changed whole-tree admissions or effects: before=%v after=%v", before, p.counts())
	}
	t.Logf("noncooperative whole-tree snapshot unchanged: %v; consumer saw tool exit", before)
}
