package eino

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
)

// The test wrapper only counts reads/writes around the production read-only
// store. The actual agent, Runner load, Name check and recover guard are all
// production code; no test-only execution implementation remains.
type p3CheckpointProbeAudit struct {
	gets, sets atomic.Int64
	probe      *checkpointValidationAgent
}

type p3CheckpointProbeCounts struct {
	Gets, Sets     int64
	Checked, Valid bool
}

func (a *p3CheckpointProbeAudit) snapshot() p3CheckpointProbeCounts {
	return p3CheckpointProbeCounts{a.gets.Load(), a.sets.Load(), a.probe.checked, a.probe.valid}
}

func TestP3CheckpointValidationProductionSignalGuard(t *testing.T) {
	stop := &checkpointValidationStop{marker: 1}
	foreign := &checkpointValidationStop{marker: 1}
	for _, tc := range []struct {
		name           string
		signal         any
		checked, valid bool
		accepted       bool
	}{
		{"same-object-checked-valid", stop, true, true, true},
		{"different-object-checked-valid", foreign, true, true, false},
		{"same-object-unchecked", stop, false, true, false},
		{"same-object-invalid", stop, true, false, false},
		{"normal-fallthrough-checked-valid", nil, true, true, false},
		{"other-panic-checked-valid", errors.New("synthetic foreign panic"), true, true, false},
		{"uncomparable-panic-checked-valid", []byte("synthetic panic"), true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkpointValidationResult(tc.signal, stop, tc.checked, tc.valid)
			if tc.accepted {
				if err != nil {
					t.Fatal(err)
				}
			} else if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleResume {
				t.Fatalf("unexpected signal acceptance: %v", err)
			}
		})
	}
}

func p3CheckpointProductionProbe(data []byte, targetName string, audit *p3CheckpointProbeAudit) error {
	audit.probe = &checkpointValidationAgent{name: targetName}
	return audit.probe.validate(p3CheckpointCountingStore{
		checkpointValidationStore: checkpointValidationStore{data: bytes.Clone(data)}, audit: audit,
	})
}

type p3CheckpointCountingStore struct {
	checkpointValidationStore
	audit *p3CheckpointProbeAudit
}

func (s p3CheckpointCountingStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	s.audit.gets.Add(1)
	return s.checkpointValidationStore.Get(ctx, key)
}

func (s p3CheckpointCountingStore) Set(ctx context.Context, key string, data []byte) error {
	s.audit.sets.Add(1)
	return s.checkpointValidationStore.Set(ctx, key, data)
}

func TestP3CheckpointValidationProductionFallbacksReject(t *testing.T) {
	probe := &checkpointValidationAgent{name: "main"}
	for _, iter := range []*adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]{
		probe.Run(t.Context(), &adk.TypedAgentInput[*schema.AgenticMessage]{}),
		probe.Resume(t.Context(), &adk.ResumeInfo{WasInterrupted: true, InterruptState: []byte("state")}),
	} {
		event, ok := iter.Next()
		if !ok || event == nil || event.Output != nil || event.Action != nil {
			t.Fatal("fallback must emit only a refusal")
		}
		if pe, ok := product.AsError(event.Err); !ok || pe.Code != product.CodeIncompatibleResume {
			t.Fatalf("fallback error=%v", event.Err)
		}
		if _, ok := iter.Next(); ok {
			t.Fatal("fallback emitted additional execution")
		}
	}
	store := checkpointValidationStore{data: []byte("private native bytes")}
	copy, ok, err := store.Get(t.Context(), "validate")
	if !ok || err != nil {
		t.Fatal("read-only store did not return its own copy")
	}
	copy[0] = 'X'
	if store.data[0] == 'X' {
		t.Fatal("read-only store exposed owned bytes")
	}
	if pe, ok := product.AsError(store.Set(t.Context(), "validate", copy)); !ok || pe.Code != product.CodeIncompatibleResume {
		t.Fatal("read-only store permitted a write")
	}
}

// Fixtures are saved by the real pinned Runner using public interruption and
// session-value APIs, never by constructing or decoding private native shapes.
type p3CheckpointFixtureAgent struct {
	name          string
	state         any
	complete      bool
	runs, resumes atomic.Int64
}

func (a *p3CheckpointFixtureAgent) Name(context.Context) string      { return a.name }
func (*p3CheckpointFixtureAgent) Description(context.Context) string { return "Step8 offline fixture" }
func (a *p3CheckpointFixtureAgent) Run(ctx context.Context, _ *adk.TypedAgentInput[*schema.AgenticMessage], _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]] {
	a.runs.Add(1)
	iter, gen := adk.NewAsyncIteratorPair[*adk.TypedAgentEvent[*schema.AgenticMessage]]()
	if a.complete {
		message := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "normal Step8 execution"})}}
		gen.Send(adk.EventFromAgenticMessage(message, nil, schema.AgenticRoleTypeAssistant))
	} else {
		gen.Send(adk.TypedStatefulInterrupt[*schema.AgenticMessage](ctx, "Step8 fixture interruption", a.state))
	}
	gen.Close()
	return iter
}
func (a *p3CheckpointFixtureAgent) Resume(context.Context, *adk.ResumeInfo, ...adk.AgentRunOption) *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]] {
	a.resumes.Add(1)
	return checkpointValidationIterator()
}

type p3CheckpointFixture struct {
	native []byte
	agent  *p3CheckpointFixtureAgent
	store  *p3WorkflowStore
}

func p3CheckpointSaveFixture(t *testing.T, name string, state any, values map[string]any) p3CheckpointFixture {
	t.Helper()
	a := &p3CheckpointFixtureAgent{name: name, state: state}
	s := &p3WorkflowStore{}
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: a, CheckPointStore: s})
	iter := runner.Run(t.Context(), []*schema.AgenticMessage{schema.UserAgenticMessage("offline checkpoint fixture")}, adk.WithCheckPointID("fixture"), adk.WithSessionValues(values))
	interruptions := 0
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event == nil || event.Err != nil {
			t.Fatalf("fixture execution failed: %v", event)
		}
		if event.Action != nil && event.Action.Interrupted != nil {
			interruptions++
		}
	}
	data, ok, err := s.Get(t.Context(), "fixture")
	if err != nil || !ok || len(data) == 0 || interruptions != 1 || a.runs.Load() != 1 || a.resumes.Load() != 0 {
		t.Fatalf("real native fixture not saved: exists=%t interruptions=%d runs=%d resumes=%d err=%v", ok, interruptions, a.runs.Load(), a.resumes.Load(), err)
	}
	if _, sets := s.counts(); sets != 1 {
		t.Fatalf("fixture saves=%d, want 1", sets)
	}
	return p3CheckpointFixture{native: data, agent: a, store: s}
}

func p3CheckpointProductionAssert(t *testing.T, data []byte, target string, valid, checked bool, handler *p3CheckpointCallbackHandler) *p3CheckpointProbeAudit {
	t.Helper()
	before := handler.snapshot()
	original := bytes.Clone(data)
	audit := &p3CheckpointProbeAudit{}
	err := p3CheckpointProductionProbe(data, target, audit)
	if valid {
		if err != nil {
			t.Fatalf("valid checkpoint rejected: %v", err)
		}
	} else if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleResume {
		t.Fatalf("unsafe checkpoint must reject with incompatible_resume: %v", err)
	}
	counts := audit.snapshot()
	gets := int64(1)
	if target == "" {
		gets = 0
	}
	want := p3CheckpointProbeCounts{Gets: gets, Checked: checked, Valid: valid}
	if counts != want {
		t.Fatalf("production preflight counts=%+v, want %+v", counts, want)
	}
	if delta := handler.snapshot().minus(before); delta != (p3CheckpointCallbackCounts{}) {
		t.Fatalf("production preflight invoked lifecycle callbacks or TimingChecker.Needed: %+v", delta)
	}
	if !bytes.Equal(data, original) {
		t.Fatal("production preflight changed caller-owned native bytes")
	}
	t.Logf("production preflight valid=%t counts=%+v lifecycle_callbacks=0 Needed=0", valid, counts)
	return audit
}

func TestP3CheckpointValidationProductionNativeMatrix(t *testing.T) {
	const name = "TestP3CheckpointValidationProductionNativeMatrix"
	mode := os.Getenv(p3CheckpointCallbackChildEnv)
	if !strings.HasPrefix(mode, "matrix:") {
		for _, timing := range []string{"count", "start-panic", "end-panic", "needed-panic", "error-panic", "stream-start-panic", "stream-end-panic"} {
			t.Run(timing, func(t *testing.T) {
				p3CheckpointCallbackSubprocess(t, name, "matrix:"+timing)
			})
		}
		return
	}
	handler := p3CheckpointCallbackRegister(strings.TrimPrefix(mode, "matrix:"))
	main := p3CheckpointSaveFixture(t, "main", []byte("main native state"), nil)
	nonMain := p3CheckpointSaveFixture(t, "step8-custom-agent", []byte("non-main native state"), nil)
	missing := p3CheckpointSaveFixture(t, "main", nil, nil)
	wrongType := p3CheckpointSaveFixture(t, "main", "not byte state", nil)
	empty := p3CheckpointSaveFixture(t, "main", []byte{}, nil)
	nilBytes := p3CheckpointSaveFixture(t, "main", []byte(nil), nil)
	h := newCheckpointHarness(true)
	t.Cleanup(h.closeRelease)
	loop, cancel := h.startLoop(t, false, "")
	if ok, _ := loop.Push(checkpointPrompt); !ok {
		t.Fatal("Push rejected")
	}
	h.waitSignal(t, loop, cancel, h.model.started, "product fixture model did not enter")
	loop.Stop(adk.WithGraceful())
	assertCancelCheckpoint(t, h.waitLoop(t, loop, cancel), h.store, h.cpID)
	productEnvelope := p2ReadEnvelope(t, h.store, h.cpID)
	beforeBudget := h.budget.Snapshot()
	handler.drains.Wait()
	handler.armed.Store(true)
	var oldPayload bytes.Buffer
	// Public info-only legacy-style payload, not Eino's native gob structure.
	if err := gob.NewEncoder(&oldPayload).Encode(adk.ResumeInfo{InterruptInfo: &adk.InterruptInfo{Data: "legacy info-only payload"}, WasInterrupted: true, InterruptState: []byte("legacy state")}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, target string
		data         []byte
		valid        bool
		checked      bool
	}{
		{"main", "main", main.native, true, true},
		{"actual-product-pause", "main", productEnvelope.RunnerCheckpoint, true, true},
		{"non-main", "step8-custom-agent", nonMain.native, true, true},
		{"wrong-target", "another-agent", main.native, false, true},
		{"empty-target", "", main.native, false, false},
		{"missing-state", "main", missing.native, false, true},
		{"wrong-state-type", "main", wrongType.native, false, true},
		{"empty-state", "main", empty.native, false, true},
		{"nil-byte-state", "main", nilBytes.native, false, true},
		{"corrupt-native", "main", []byte("corrupt native payload"), false, false},
		{"truncated-native", "main", main.native[:len(main.native)/2], false, false},
		{"empty-native", "main", nil, false, false},
		{"legacy-info-only", "main", oldPayload.Bytes(), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p3CheckpointProductionAssert(t, tc.data, tc.target, tc.valid, tc.checked, handler)
		})
	}
	// Preserve the repository's actual earlier scalar checkpoint. Decode only
	// the product-owned TurnLoop envelope, leaving its native bytes opaque.
	t.Run("existing-legacy-scalar", func(t *testing.T) {
		encoded, err := os.ReadFile("testdata/checkpoint_scalar_v0.9.21.b64")
		if err != nil {
			t.Fatal(err)
		}
		blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
		if err != nil {
			t.Fatal(err)
		}
		var envelope p2CheckpointEnvelope
		if err := gob.NewDecoder(bytes.NewReader(blob)).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		p3CheckpointProductionAssert(t, envelope.RunnerCheckpoint, "main", true, true, handler)
	})
	if h.fake.Calls() != 1 || h.model.before.Load() != 1 || h.model.later.Load() != 0 || h.tool.total() != 0 || h.budget.Snapshot() != beforeBudget {
		t.Fatal("production preflight repeated original product business execution or changed its budget")
	}
	for _, fixture := range []p3CheckpointFixture{main, nonMain, missing, wrongType, empty, nilBytes} {
		if _, sets := fixture.store.counts(); sets != 1 || fixture.agent.runs.Load() != 1 || fixture.agent.resumes.Load() != 0 {
			t.Fatal("production preflight repeated fixture business execution or checkpoint save")
		}
		stored, ok, err := fixture.store.Get(t.Context(), "fixture")
		if err != nil || !ok || !bytes.Equal(stored, fixture.native) {
			t.Fatal("production preflight modified the original native store")
		}
	}
}
