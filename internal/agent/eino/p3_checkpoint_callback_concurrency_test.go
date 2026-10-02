package eino

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
)

// These are host-owned codecs registered through the public serialization API.
// They live in session values, so a native load must call the codec before Name.
func init() {
	schema.Register[*p3CheckpointGobBarrier]()
	schema.Register[*p3CheckpointBinaryBarrier]()
}

type p3CheckpointGobBarrier struct{ ID string }

func (b *p3CheckpointGobBarrier) GobEncode() ([]byte, error) { return []byte(b.ID), nil }
func (b *p3CheckpointGobBarrier) GobDecode(data []byte) error {
	b.ID = string(data)
	return p3CheckpointCodecEnter(b.ID)
}

type p3CheckpointBinaryBarrier struct{ ID string }

func (b *p3CheckpointBinaryBarrier) MarshalBinary() ([]byte, error) { return []byte(b.ID), nil }
func (b *p3CheckpointBinaryBarrier) UnmarshalBinary(data []byte) error {
	b.ID = string(data)
	return p3CheckpointCodecEnter(b.ID)
}

type p3CheckpointCodecGate struct {
	entered, release chan struct{}
	enterOnce        sync.Once
	releaseOnce      sync.Once
	calls            atomic.Int64
	panicValue       any
	panics           bool
}

func (g *p3CheckpointCodecGate) unblock() { g.releaseOnce.Do(func() { close(g.release) }) }

var p3CheckpointCodecGates sync.Map

func p3CheckpointCodecEnter(id string) error {
	value, ok := p3CheckpointCodecGates.Load(id)
	if !ok {
		return errors.New("Step8 codec gate is missing")
	}
	gate := value.(*p3CheckpointCodecGate)
	gate.calls.Add(1)
	gate.enterOnce.Do(func() { close(gate.entered) })
	<-gate.release
	if gate.panics {
		panic(gate.panicValue)
	}
	return nil
}

func p3CheckpointNewCodecGate(t *testing.T, id string) *p3CheckpointCodecGate {
	t.Helper()
	gate := &p3CheckpointCodecGate{entered: make(chan struct{}), release: make(chan struct{})}
	if _, loaded := p3CheckpointCodecGates.LoadOrStore(id, gate); loaded {
		t.Fatal("codec fixture reused an active gate")
	}
	t.Cleanup(func() {
		gate.unblock()
		p3CheckpointCodecGates.Delete(id)
	})
	return gate
}

func p3CheckpointCodecValue(codec, id string) any {
	if codec == "gob" {
		return &p3CheckpointGobBarrier{ID: id}
	}
	return &p3CheckpointBinaryBarrier{ID: id}
}

// This key is owned solely by the tests. It neither inspects nor shadows an
// Eino private context key; the preflight continues to use context.Background.
type p3CheckpointNormalScopeKey struct{}
type p3CheckpointNormalScope struct {
	needed               atomic.Int64
	timings              [5]atomic.Int64
	initialized, release chan struct{}
	once, releaseOnce    sync.Once
}

func (n *p3CheckpointNormalScope) snapshot() p3CheckpointCallbackCounts {
	counts := p3CheckpointCallbackCounts{Needed: n.needed.Load()}
	for i := range counts.Timings {
		counts.Timings[i] = n.timings[i].Load()
	}
	return counts
}

func (n *p3CheckpointNormalScope) unblock() { n.releaseOnce.Do(func() { close(n.release) }) }

func p3CheckpointWait(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal(message)
	}
}

func p3CheckpointWaitError(t *testing.T, result <-chan error, message string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal(message)
		return nil
	}
}

func p3CheckpointNormalRun(ctx context.Context, a *p3CheckpointFixtureAgent) error {
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: a})
	iter := runner.Run(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("normal offline execution")})
	outputs := 0
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event == nil || event.Err != nil {
			return errors.New("normal Runner failed")
		}
		if event.Output != nil && event.Output.MessageOutput != nil {
			outputs++
		}
	}
	if outputs != 1 || a.runs.Load() != 1 || a.resumes.Load() != 0 {
		return fmt.Errorf("normal Runner outputs=%d Run=%d Resume=%d", outputs, a.runs.Load(), a.resumes.Load())
	}
	return nil
}

func p3CheckpointNormalAllTimings(t *testing.T, handler *p3CheckpointCallbackHandler) {
	t.Helper()
	normal := &p3CheckpointNormalScope{}
	ctx := context.WithValue(t.Context(), p3CheckpointNormalScopeKey{}, normal)
	ctx = callbacks.InitCallbacks(ctx, &callbacks.RunInfo{Name: "step8-normal-hooks", Component: adk.ComponentOfAgenticAgent})
	ctx = callbacks.OnStart(ctx, "normal start")
	ctx = callbacks.OnError(ctx, errors.New("synthetic normal error timing"))
	ctx, input := callbacks.OnStartWithStreamInput(ctx, schema.StreamReaderFromArray([]callbacks.CallbackInput{"normal input"}))
	input.Close()
	ctx, output := callbacks.OnEndWithStreamOutput(ctx, schema.StreamReaderFromArray([]callbacks.CallbackOutput{"normal output"}))
	output.Close()
	callbacks.OnEnd(ctx, "normal end")
	handler.drains.Wait()
	if counts := normal.snapshot(); counts != (p3CheckpointCallbackCounts{Needed: 5, Timings: [5]int64{1, 1, 1, 1, 1}}) {
		t.Fatalf("normal public callback hooks were suppressed: %+v", counts)
	}
	t.Logf("normal public hooks retained all timings: %+v", normal.snapshot())
}

func TestP3CheckpointValidationProductionNativeCallbackOverlap(t *testing.T) {
	const name = "TestP3CheckpointValidationProductionNativeCallbackOverlap"
	mode := os.Getenv(p3CheckpointCallbackChildEnv)
	if !strings.HasPrefix(mode, "overlap:") {
		for _, codec := range []string{"gob", "binary"} {
			t.Run(codec, func(t *testing.T) { p3CheckpointCallbackSubprocess(t, name, "overlap:"+codec) })
		}
		return
	}
	handler := p3CheckpointCallbackRegister("count")
	codec := strings.TrimPrefix(mode, "overlap:")
	gate := p3CheckpointNewCodecGate(t, codec+"-overlap")
	fixture := p3CheckpointSaveFixture(t, "main", []byte("overlap native state"), map[string]any{"step8-codec": p3CheckpointCodecValue(codec, codec+"-overlap")})
	handler.drains.Wait()
	before := handler.snapshot()
	audit := &p3CheckpointProbeAudit{}
	preflightDone := make(chan error, 1)
	go func() { preflightDone <- p3CheckpointProductionProbe(fixture.native, "main", audit) }()
	p3CheckpointWait(t, gate.entered, "native loader did not enter the host codec")

	normal := &p3CheckpointNormalScope{initialized: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(normal.unblock)
	ctx := context.WithValue(t.Context(), p3CheckpointNormalScopeKey{}, normal)
	normalAgent := &p3CheckpointFixtureAgent{name: "step8-normal", complete: true}
	normalDone := make(chan error, 1)
	go func() { normalDone <- p3CheckpointNormalRun(ctx, normalAgent) }()
	// Needed is reached only after the real Runner initializes its callback
	// manager. Hold it there while preflight finishes; no sleep/retry sampling.
	p3CheckpointWait(t, normal.initialized, "normal Runner did not initialize lifecycle callbacks")
	gate.unblock()
	if err := p3CheckpointWaitError(t, preflightDone, "production preflight did not exit Name"); err != nil {
		t.Fatalf("valid overlapped production preflight failed: %v", err)
	}
	if counts := audit.snapshot(); counts != (p3CheckpointProbeCounts{Gets: 1, Checked: true, Valid: true}) {
		t.Fatalf("overlap production preflight counts=%+v", counts)
	}
	if delta := handler.snapshot().minus(before).minus(normal.snapshot()); delta != (p3CheckpointCallbackCounts{}) {
		t.Fatalf("preflight invoked callbacks during normal initialization: %+v", delta)
	}
	normal.unblock()
	if err := p3CheckpointWaitError(t, normalDone, "normal Runner did not finish"); err != nil {
		t.Fatal(err)
	}
	handler.drains.Wait()
	if counts := normal.snapshot(); counts != (p3CheckpointCallbackCounts{Needed: 2, Timings: [5]int64{1, 1, 0, 0, 0}}) {
		t.Fatalf("normal Runner callbacks were suppressed: %+v", counts)
	}
	if delta := handler.snapshot().minus(before).minus(normal.snapshot()); delta != (p3CheckpointCallbackCounts{}) {
		t.Fatalf("preflight added lifecycle callbacks: %+v", delta)
	}
	if _, sets := fixture.store.counts(); sets != 1 || gate.calls.Load() != 1 || fixture.agent.runs.Load() != 1 || fixture.agent.resumes.Load() != 0 {
		t.Fatal("overlap changed original business/save counts or did not actually load the codec")
	}
	t.Logf("%s deterministic overlap: codec=1 preflight=%+v callback_delta=0 normal_Runner=%+v outputs=1", codec, audit.snapshot(), normal.snapshot())
	p3CheckpointNormalAllTimings(t, handler)
}

func TestP3CheckpointValidationProductionNativeConcurrentSignals(t *testing.T) {
	const name = "TestP3CheckpointValidationProductionNativeConcurrentSignals"
	if os.Getenv(p3CheckpointCallbackChildEnv) != "concurrent-signals" {
		p3CheckpointCallbackSubprocess(t, name, "concurrent-signals")
		return
	}
	handler := p3CheckpointCallbackRegister("count")
	type invocation struct {
		id, target string
		valid      bool
		checked    bool
		fixture    p3CheckpointFixture
		gate       *p3CheckpointCodecGate
		audit      *p3CheckpointProbeAudit
		done       chan error
	}
	var invocations []invocation
	for i, tc := range []struct {
		id, codec, original, target string
		state                       any
		valid                       bool
		checked                     bool
	}{
		{"valid-main", "gob", "main", "main", []byte("main state"), true, true},
		{"valid-custom", "binary", "custom", "custom", []byte("custom state"), true, true},
		{"wrong-target", "gob", "main", "wrong", []byte("main state"), false, true},
		{"missing-state", "binary", "main", "main", nil, false, true},
		{"foreign-signal", "gob", "main", "main", []byte("main state"), false, false},
		{"uncomparable-panic", "binary", "main", "main", []byte("main state"), false, false},
		{"nil-panic", "gob", "main", "main", []byte("main state"), false, false},
	} {
		gate := p3CheckpointNewCodecGate(t, tc.id)
		if i == 5 {
			gate.panics, gate.panicValue = true, []byte("synthetic uncomparable codec panic")
		}
		if i == 6 {
			gate.panics = true
		}
		fixture := p3CheckpointSaveFixture(t, tc.original, tc.state, map[string]any{"step8-codec": p3CheckpointCodecValue(tc.codec, tc.id)})
		invocations = append(invocations, invocation{tc.id, tc.target, tc.valid, tc.checked, fixture, gate, &p3CheckpointProbeAudit{}, make(chan error, 1)})
	}
	handler.drains.Wait()
	before := handler.snapshot()
	for _, call := range invocations {
		go func() { call.done <- p3CheckpointProductionProbe(call.fixture.native, call.target, call.audit) }()
	}
	for _, call := range invocations {
		p3CheckpointWait(t, call.gate.entered, "concurrent validation did not load its own codec")
	}
	// All invocations are simultaneously inside native loading. Borrow the
	// real successful invocation's sentinel for a different loader's panic.
	// Channel release below establishes synchronization for this codec input.
	invocations[4].gate.panics = true
	invocations[4].gate.panicValue = invocations[0].audit.probe.stop
	seen := make(map[*checkpointValidationStop]bool)
	for _, call := range invocations {
		if call.audit.probe.stop == nil || call.audit.probe.stop.marker != 1 || seen[call.audit.probe.stop] {
			t.Fatal("concurrent validations reused a sentinel object")
		}
		seen[call.audit.probe.stop] = true
		call.gate.unblock()
	}
	for _, call := range invocations {
		err := p3CheckpointWaitError(t, call.done, "concurrent validation did not finish")
		if call.valid {
			if err != nil {
				t.Fatalf("valid concurrent %s rejected: %v", call.id, err)
			}
		} else if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleResume {
			t.Fatalf("unsafe concurrent %s accepted: %v", call.id, err)
		}
		want := p3CheckpointProbeCounts{Gets: 1, Checked: call.checked, Valid: call.valid}
		if counts := call.audit.snapshot(); counts != want || call.gate.calls.Load() != 1 {
			t.Fatalf("concurrent %s counts=%+v codec=%d, want %+v codec=1", call.id, counts, call.gate.calls.Load(), want)
		}
		if _, sets := call.fixture.store.counts(); sets != 1 || call.fixture.agent.runs.Load() != 1 || call.fixture.agent.resumes.Load() != 0 {
			t.Fatal("concurrent validation repeated original business or save work")
		}
		t.Logf("concurrent %s valid=%t counts=%+v codec=1", call.id, call.valid, call.audit.snapshot())
	}
	if delta := handler.snapshot().minus(before); delta != (p3CheckpointCallbackCounts{}) {
		t.Fatalf("concurrent production preflight invoked callbacks: %+v", delta)
	}
	t.Log("seven distinct per-call sentinels; borrowed/other/nil codec panics rejected; all lifecycle callbacks=0 Needed=0")
}
