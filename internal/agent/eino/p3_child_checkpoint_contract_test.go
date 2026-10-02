package eino

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

func TestP3ChildResumeContractCheckpointErrors(t *testing.T) {
	for _, failure := range []string{"missing", "corrupt", "read"} {
		t.Run(failure, func(t *testing.T) {
			p := newP3ChildProbe()
			ctx := WithExecutionScope(t.Context(), p.scope)
			store := &p3WorkflowStore{}
			sentinel := errors.New("synthetic child checkpoint failure")
			makeRunner := func(store *p3WorkflowStore) *adk.TypedRunner[*schema.AgenticMessage] {
				ag, err := p.newAgent(ctx, "root")
				if err != nil {
					t.Fatal(err)
				}
				return adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag, CheckPointStore: store})
			}
			contexts := p3DrainChildProbe(t, makeRunner(store).Run(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("task:root")}, adk.WithCheckPointID("tree")))
			p3ProbeRoots(t, contexts, p3FixedAddresses("leaf", "b"))
			reopened := &p3WorkflowStore{}
			if failure == "read" || failure == "corrupt" {
				blob, ok, err := store.Get(ctx, "tree")
				if err != nil || !ok {
					t.Fatal("checkpoint setup failed")
				}
				if failure == "corrupt" {
					blob = []byte("invalid synthetic child checkpoint")
				}
				if err := reopened.Set(ctx, "tree", blob); err != nil {
					t.Fatal(err)
				}
				if failure == "read" {
					reopened.getErr = sentinel
				}
			}
			before := p.counts()
			it, resumeErr := makeRunner(reopened).ResumeWithParams(ctx, "tree", &adk.ResumeParams{Targets: map[string]any{}})
			if resumeErr == nil || it != nil {
				t.Fatal("unavailable checkpoint started a resumed child runner")
			}
			if failure == "read" && !errors.Is(resumeErr, sentinel) {
				t.Fatal("checkpoint read failure lost its cause")
			}
			if !reflect.DeepEqual(p.counts(), before) || p.done.Load() != 1 || p.a.Load() != 0 || p.b.Load() != 0 {
				t.Fatal("checkpoint failure changed model/tool admissions or effects")
			}
			for _, m := range p.models {
				if m.Calls() != 1 {
					t.Fatal("failed checkpoint resume issued another fake model request")
				}
			}
			t.Logf("top-level %s rejected before new work: %v", failure, before)
		})
	}
}

// This wrapper belongs only to save-failure probes. Its counters distinguish
// this attempted commit from an older valid blob still held by the store.
type p3ChildSaveStore struct {
	base                          p3WorkflowStore
	mu                            sync.Mutex
	fail                          error
	attempts, committed, failures int
	failedCandidate               []byte
}

func (s *p3ChildSaveStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	return s.base.Get(ctx, key)
}

func (s *p3ChildSaveStore) Set(ctx context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.fail != nil {
		s.failures++
		s.failedCandidate = bytes.Clone(data)
		return s.fail
	}
	if err := s.base.Set(ctx, key, data); err != nil {
		return err
	}
	s.committed++
	return nil
}

func (s *p3ChildSaveStore) failSaves(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

func (s *p3ChildSaveStore) snapshot() (attempts, committed, failures int, candidate []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts, s.committed, s.failures, bytes.Clone(s.failedCandidate)
}

func p3CollectSaveEvents(it *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]) ([]*adk.InterruptCtx, error, []string) {
	var interrupts []*adk.InterruptCtx
	var runErr error
	var order []string
	for {
		ev, ok := it.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			order = append(order, "error")
			runErr = errors.Join(runErr, ev.Err)
		}
		if ev.Output != nil && ev.Output.MessageOutput != nil {
			_, err := ev.Output.MessageOutput.GetMessage()
			runErr = errors.Join(runErr, err)
		}
		if ev.Action != nil && ev.Action.Interrupted != nil {
			order = append(order, "interrupt")
			interrupts = ev.Action.Interrupted.InterruptContexts
		}
	}
	return interrupts, runErr, order
}

func TestP3ChildResumeContractCheckpointSaveErrorOrder(t *testing.T) {
	for _, priorBlob := range []bool{false, true} {
		t.Run(fmt.Sprintf("prior_blob_%t", priorBlob), func(t *testing.T) {
			p := newP3ChildProbe()
			ctx := WithExecutionScope(t.Context(), p.scope)
			store := &p3ChildSaveStore{}
			makeRunner := func() *adk.TypedRunner[*schema.AgenticMessage] {
				ag, err := p.newAgent(ctx, "root")
				if err != nil {
					t.Fatal(err)
				}
				return adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag, CheckPointStore: store})
			}
			var oldBlob []byte
			var ids map[string]string
			if priorBlob {
				contexts := p3DrainChildProbe(t, makeRunner().Run(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("task:root")}, adk.WithCheckPointID("tree")))
				ids = p3ProbeRoots(t, contexts, p3FixedAddresses("leaf", "b"))
				blob, ok, err := store.Get(ctx, "tree")
				if err != nil || !ok || len(blob) == 0 {
					t.Fatal("prior valid native blob missing")
				}
				oldBlob = blob
			}
			beforeAttempts, beforeCommitted, beforeFailures, _ := store.snapshot()
			sentinel := errors.New("synthetic child checkpoint save failure")
			store.failSaves(sentinel)
			var it *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]
			if priorBlob {
				var err error
				it, err = makeRunner().ResumeWithParams(ctx, "tree", &adk.ResumeParams{Targets: map[string]any{ids["approve:leaf"]: "allowed-once"}})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				it = makeRunner().Run(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("task:root")}, adk.WithCheckPointID("tree"))
			}
			contexts, runErr, order := p3CollectSaveEvents(it)
			if !errors.Is(runErr, sentinel) || !reflect.DeepEqual(order, []string{"error", "interrupt"}) {
				t.Fatalf("save failure must remain error-then-interrupt: order=%v err=%v", order, runErr)
			}
			expectedRoots := p3FixedAddresses("leaf", "b")
			if priorBlob {
				expectedRoots = p3FixedAddresses("b")
			}
			p3ProbeRoots(t, contexts, expectedRoots)
			attempts, committed, failures, candidate := store.snapshot()
			if attempts-beforeAttempts != 1 || failures-beforeFailures != 1 || committed != beforeCommitted || len(candidate) == 0 {
				t.Fatal("save failure did not attempt exactly one uncommitted native blob")
			}
			blob, exists, err := store.Get(ctx, "tree")
			if err != nil || exists != priorBlob || !bytes.Equal(blob, oldBlob) || bytes.Equal(blob, candidate) {
				t.Fatal("failed save was mistaken for a new successful blob or replaced the old blob")
			}
			wantCompleted, wantLeafModels, wantChildEntries := int32(0), 1, int32(1)
			if priorBlob {
				wantCompleted, wantLeafModels, wantChildEntries = 1, 2, 2
			}
			if p.a.Load() != wantCompleted || p.b.Load() != 0 || p.done.Load() != 1 || p.doneEntries.Load() != 1 || p.models["root"].Calls() != 1 || p.models["b"].Calls() != 1 || p.models["a"].Calls() != wantLeafModels || p.models["leaf"].Calls() != wantLeafModels {
				t.Fatal("failed save changed authorized model/effect counts")
			}
			if p.models["a"].delegations.Load() != wantChildEntries || p.models["leaf"].delegations.Load() != wantChildEntries || p.models["b"].delegations.Load() != wantChildEntries || p.models["leaf"].approvalEntries.Load() != wantChildEntries || p.models["b"].approvalEntries.Load() != wantChildEntries {
				t.Fatal("failed save repeated native child/tool admissions")
			}
			t.Logf("save failure: order=%v attempted=1 committed=0 old blob retained=%t counts=%v", order, priorBlob, p.counts())
		})
	}
}

// Gob matches exported/shared fields structurally. These deliberately partial
// shapes exist only in tests and never parse Eino's private format in product
// code. A decodable blob is not proof of semantically complete native state.
type p3NativeCheckpointShape struct {
	RunCtx          *p3NativeRunContextShape
	Info            *adk.InterruptInfo
	EnableStreaming bool
}

type p3NativeRunContextShape struct {
	Session *p3NativeSessionShape
}

type p3NativeSessionShape struct {
	Values map[string]any
}

func TestP3ChildResumeContractNativeDecodableMissingContextPanics(t *testing.T) {
	for _, missing := range []string{"RunCtx", "Session"} {
		t.Run(missing, func(t *testing.T) {
			shape := p3NativeCheckpointShape{Info: &adk.InterruptInfo{}}
			if missing == "Session" {
				shape.RunCtx = &p3NativeRunContextShape{}
			}
			var encoded bytes.Buffer
			if err := gob.NewEncoder(&encoded).Encode(shape); err != nil {
				t.Fatal(err)
			}
			var decoded p3NativeCheckpointShape
			if err := gob.NewDecoder(bytes.NewReader(encoded.Bytes())).Decode(&decoded); err != nil {
				t.Fatalf("semantic-defect fixture is not decodable gob: %v", err)
			}
			if (missing == "RunCtx" && decoded.RunCtx != nil) || (missing == "Session" && (decoded.RunCtx == nil || decoded.RunCtx.Session != nil)) {
				t.Fatal("fixture did not preserve the intended semantic omission")
			}
			p := newP3ChildProbe()
			ctx := WithExecutionScope(t.Context(), p.scope)
			ag, err := p.newAgent(ctx, "root")
			if err != nil {
				t.Fatal(err)
			}
			store := &p3WorkflowStore{}
			if err := store.Set(ctx, "tree", encoded.Bytes()); err != nil {
				t.Fatal(err)
			}
			runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag, CheckPointStore: store})
			before := p.counts()
			var panicValue any
			var it *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]
			var resumeErr error
			func() {
				defer func() { panicValue = recover() }()
				it, resumeErr = runner.ResumeWithParams(ctx, "tree", &adk.ResumeParams{Targets: map[string]any{}})
			}()
			if nativePanic, nilDereference := panicValue.(runtime.Error); !nilDereference || nativePanic.Error() != "runtime error: invalid memory address or nil pointer dereference" || it != nil || resumeErr != nil {
				t.Fatalf("native semantic-defect behavior changed; re-evaluate the adapter negative case: panic=%v iterator=%t err=%v", panicValue, it != nil, resumeErr)
			}
			if gets, sets := store.counts(); gets != 1 || sets != 1 {
				t.Fatal("semantic-defect resume did not perform exactly one load and no new save")
			}
			if !reflect.DeepEqual(p.counts(), before) {
				t.Fatal("missing native context admitted model/tool/effect work before panic")
			}
			t.Logf("KNOWN NATIVE LIMIT: decodable gob missing %s panics synchronously (%v), not safe error rejection; all invocation/model/tool/effect counts zero", missing, panicValue)
		})
	}
}
