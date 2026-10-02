package workflowagent

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/compose"
	"github.com/ww1489/seasprak/internal/testkit"
)

// Native Eino framework probes only: these do not certify product graph resume.
// This store models serialized framework checkpoints only, not product durability.
type p3WorkflowStore struct {
	mu             sync.Mutex
	data           map[string][]byte
	gets, sets     int
	getErr, setErr error
}

func (s *p3WorkflowStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.getErr != nil {
		return nil, false, s.getErr
	}
	data, ok := s.data[key]
	return bytes.Clone(data), ok, nil
}
func (s *p3WorkflowStore) Set(_ context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets++
	if s.setErr != nil {
		return s.setErr
	}
	if s.data == nil {
		s.data = make(map[string][]byte)
	}
	s.data[key] = bytes.Clone(data)
	return nil
}
func (s *p3WorkflowStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.sets
}

type p3WorkflowCalls struct{ before, wait, after atomic.Int32 }

func (c *p3WorkflowCalls) assert(t *testing.T, b, w, a int32) {
	t.Helper()
	if c.before.Load() != b || c.wait.Load() != w || c.after.Load() != a {
		t.Fatalf("node counts: before=%d wait=%d after=%d", c.before.Load(), c.wait.Load(), c.after.Load())
	}
}

func p3WorkflowGraph(t *testing.T, store *p3WorkflowStore, calls *p3WorkflowCalls, block *testkit.FakeModel, started chan<- struct{}) compose.Runnable[string, string] {
	t.Helper()
	g := compose.NewGraph[string, string]()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal("graph construction failed")
		}
	}
	check(g.AddLambdaNode("before", compose.InvokableLambda(func(_ context.Context, in string) (string, error) { calls.before.Add(1); return in + "/before", nil })))
	check(g.AddLambdaNode("wait", compose.InvokableLambda(func(ctx context.Context, in string) (string, error) {
		calls.wait.Add(1)
		if block != nil {
			close(started)
			_, err := block.Generate(ctx, nil)
			return "", err
		}
		interrupted, hasState, saved := compose.GetInterruptState[string](ctx)
		if !interrupted {
			return "", compose.StatefulInterrupt(ctx, "synthetic pause", in)
		}
		resumed, hasData, data := compose.GetResumeContext[string](ctx)
		// Internal interrupts do not retain the input argument automatically.
		// restoreTasks supplies its zero value; StatefulInterrupt owns the saved input.
		if !hasState || saved == "" || in != "" || !resumed || !hasData || data != "continue" {
			t.Errorf("resume flags: hasState=%t savedNonempty=%t inputZero=%t resumed=%t hasData=%t dataMatches=%t", hasState, saved != "", in == "", resumed, hasData, data == "continue")
			return "", errors.New("synthetic resume contract mismatch")
		}
		return saved + "/resumed", nil
	})))
	check(g.AddLambdaNode("after", compose.InvokableLambda(func(_ context.Context, in string) (string, error) { calls.after.Add(1); return in + "/after", nil })))
	for _, edge := range [][2]string{{compose.START, "before"}, {"before", "wait"}, {"wait", "after"}, {"after", compose.END}} {
		check(g.AddEdge(edge[0], edge[1]))
	}
	graph, err := g.Compile(t.Context(), compose.WithCheckPointStore(store), compose.WithGraphName("p3-contract"))
	check(err)
	return graph
}

func p3WorkflowInterruptID(t *testing.T, err error) string {
	t.Helper()
	info, ok := compose.ExtractInterruptInfo(err)
	if err == nil || !ok || info == nil || len(info.InterruptContexts) != 1 || info.InterruptContexts[0].ID == "" {
		t.Fatal("expected one resumable framework interruption")
	}
	if info.InterruptContexts[0].Address.String() != "runnable:p3-contract;node:wait" {
		t.Fatal("interrupt address mismatch")
	}
	return info.InterruptContexts[0].ID
}

func TestP3WorkflowCheckpointInterruptResume(t *testing.T) {
	store := &p3WorkflowStore{}
	calls := &p3WorkflowCalls{}
	graph := p3WorkflowGraph(t, store, calls, nil, nil)
	out, err := graph.Invoke(t.Context(), "synthetic", compose.WithCheckPointID("checkpoint"))
	id := p3WorkflowInterruptID(t, err)
	if out != "" {
		t.Fatal("interruption produced final result")
	}
	calls.assert(t, 1, 1, 0)
	if gets, sets := store.counts(); gets != 1 || sets != 1 {
		t.Fatal("initial checkpoint operation counts mismatch")
	}
	blob, ok, err := store.Get(t.Context(), "checkpoint")
	if err != nil || !ok || len(blob) == 0 {
		t.Fatal("missing serialized checkpoint")
	}
	// Restore independent bytes into a new store and recompile: no hidden
	// runnable-local cache or aliased slice can account for successful resume.
	restored := &p3WorkflowStore{}
	if restored.Set(t.Context(), "checkpoint", blob) != nil {
		t.Fatal("restore checkpoint failed")
	}
	blob[0] ^= 0xff
	resumed := p3WorkflowGraph(t, restored, calls, nil, nil)
	out, err = resumed.Invoke(compose.ResumeWithData(t.Context(), id, "continue"), "ignored-new-input", compose.WithCheckPointID("checkpoint"))
	if err != nil || out != "synthetic/before/resumed/after" {
		t.Fatalf("checkpoint resume result mismatch: errorType=%T outputMatches=%t", err, out == "synthetic/before/resumed/after")
	}
	calls.assert(t, 1, 2, 1) // Interrupted node reruns; the completed predecessor does not.
	if gets, sets := restored.counts(); gets != 1 || sets != 1 {
		t.Fatal("resume unexpectedly rewrote checkpoint")
	}
}

func TestP3WorkflowCheckpointErrors(t *testing.T) {
	sentinel := errors.New("synthetic checkpoint failure")
	for _, stage := range []string{"read", "write", "resume_read", "corrupt"} {
		t.Run(stage, func(t *testing.T) {
			store := &p3WorkflowStore{}
			calls := &p3WorkflowCalls{}
			graph := p3WorkflowGraph(t, store, calls, nil, nil)
			ctx := t.Context()
			if stage == "read" {
				store.getErr = sentinel
			}
			if stage == "write" {
				store.setErr = sentinel
			}
			if stage == "resume_read" || stage == "corrupt" {
				_, err := graph.Invoke(ctx, "synthetic", compose.WithCheckPointID("checkpoint"))
				id := p3WorkflowInterruptID(t, err)
				calls.assert(t, 1, 1, 0)
				ctx = compose.ResumeWithData(ctx, id, "continue")
				if stage == "resume_read" {
					store.getErr = sentinel
				} else {
					if store.Set(ctx, "checkpoint", []byte("invalid synthetic checkpoint")) != nil {
						t.Fatal("inject corrupt checkpoint failed")
					}
				}
			}
			out, err := graph.Invoke(ctx, "synthetic", compose.WithCheckPointID("checkpoint"))
			if err == nil || out != "" {
				t.Fatal("checkpoint failure produced successful result")
			}
			if stage != "corrupt" && !errors.Is(err, sentinel) {
				t.Fatal("checkpoint failure lost error identity")
			}
			if _, ok := compose.ExtractInterruptInfo(err); ok {
				t.Fatal("failed checkpoint advertised a resumable interruption")
			}
			switch stage {
			case "read":
				calls.assert(t, 0, 0, 0)
			case "write", "resume_read", "corrupt":
				calls.assert(t, 1, 1, 0)
			}
			gets, sets := store.counts()
			wantGets, wantSets := 1, 0
			if stage == "write" {
				wantSets = 1
			}
			if stage == "resume_read" {
				wantGets, wantSets = 2, 1
			}
			if stage == "corrupt" {
				wantGets, wantSets = 2, 2
			}
			if gets != wantGets || sets != wantSets {
				t.Fatal("checkpoint failure invocation counts mismatch")
			}
			if stage == "write" {
				_, ok, _ := store.Get(ctx, "checkpoint")
				if ok {
					t.Fatal("failed write retained checkpoint")
				}
			}
		})
	}
}

func TestP3WorkflowCancellationDoesNotBecomeInterrupt(t *testing.T) {
	store := &p3WorkflowStore{}
	calls := &p3WorkflowCalls{}
	model := testkit.NewFake(testkit.Step{Gate: make(chan struct{})})
	started := make(chan struct{})
	graph := p3WorkflowGraph(t, store, calls, model, started)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := graph.Invoke(ctx, "synthetic", compose.WithCheckPointID("checkpoint"))
		done <- result{out, err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking node did not start")
	}
	cancel()
	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled graph did not exit")
	}
	if !errors.Is(got.err, context.Canceled) || got.out != "" {
		t.Fatal("cancellation returned success or lost cancellation identity")
	}
	if _, ok := compose.ExtractInterruptInfo(got.err); ok {
		t.Fatal("cancellation unexpectedly became resumable interrupt")
	}
	calls.assert(t, 1, 1, 0)
	if model.Calls() != 1 {
		t.Fatal("cancelled model was repeated")
	}
	if gets, sets := store.counts(); gets != 1 || sets != 0 {
		t.Fatal("cancellation unexpectedly saved checkpoint")
	}
}

func TestP3WorkflowCheckpointStoreOwnsBytes(t *testing.T) {
	store := &p3WorkflowStore{}
	data := []byte{1, 2, 3}
	if store.Set(t.Context(), "checkpoint", data) != nil {
		t.Fatal("store set failed")
	}
	data[0] = 9
	first, ok, err := store.Get(t.Context(), "checkpoint")
	if err != nil || !ok || !bytes.Equal(first, []byte{1, 2, 3}) {
		t.Fatal("Set retained caller byte alias")
	}
	first[1] = 9
	second, ok, err := store.Get(t.Context(), "checkpoint")
	if err != nil || !ok || !bytes.Equal(second, []byte{1, 2, 3}) {
		t.Fatal("Get exposed stored byte alias")
	}
}
