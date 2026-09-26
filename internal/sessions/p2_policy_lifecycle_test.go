package sessions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func policyCode(t *testing.T, err error, code string) {
	t.Helper()
	pe, ok := product.AsError(err)
	if !ok || pe.Code != code {
		t.Fatalf("error=%v want=%s", err, code)
	}
}

func TestP2PolicyDefaultsCopyAndChangesCommit(t *testing.T) {
	backend, err := memory.Open("policy-copy", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "policy-copy")
	if err != nil {
		t.Fatal(err)
	}
	configured := agent.ResolvedPolicy{Ref: "forged", Revision: 99}
	s, err := Start(Options{SessionID: "policy-copy", Profile: ProfileMemory, Model: testkit.NewFake(), Store: backend, Policy: &configured}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	initial := manager.View().ExecutionPolicy
	if initial.Ref == "" || initial.Ref == configured.Ref || initial.Revision != 1 || initial.SandboxMode != "workspace-write" || initial.ApprovalPolicy != "ask" || initial.Auto {
		t.Fatalf("policy=%+v", initial)
	}
	configured.SandboxMode = "danger-full-access"
	configured.Auto = true
	if *s.rt.opts.Policy == configured || manager.View().ExecutionPolicy != initial {
		t.Fatal("configuration pointer aliased")
	}
	sub := s.SubscribeEvents(s.rt.opts.Limits)
	defer sub.Close()
	if err := s.rt.setExecutionPolicy(t.Context(), 1, agent.ResolvedPolicy{SandboxMode: "read-only", ApprovalPolicy: "never"}); err != nil {
		t.Fatal(err)
	}
	current := manager.View().ExecutionPolicy
	if current.Revision != 2 || current.Ref == initial.Ref || current.SandboxMode != "read-only" || current.ApprovalPolicy != "never" {
		t.Fatalf("policy=%+v", current)
	}
	select {
	case ev := <-sub.Events:
		if ev.Type != "security.policy_changed" || ev.DurableSeq == nil {
			t.Fatalf("event=%+v", ev)
		}
		var p agent.ResolvedPolicy
		_ = json.Unmarshal(ev.Payload, &p)
		if p != current {
			t.Fatal("event differs from committed policy")
		}
	case <-t.Context().Done():
		t.Fatal("policy event not published")
	}
	before := manager.View().LastSeq
	policyCode(t, s.rt.setExecutionPolicy(t.Context(), 1, agent.ResolvedPolicy{}), product.CodeStateConflict)
	if manager.View().LastSeq != before {
		t.Fatal("stale policy changed journal")
	}
}

func TestP2PolicyStartAppendFailureDoesNotStart(t *testing.T) {
	backend, err := memory.Open("policy-start", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	faults := &p2AttemptFaultStore{Store: backend, kind: "execution_policy", failed: make(chan struct{})}
	manager, err := state.NewManager(faults, "policy-start")
	if err != nil {
		t.Fatal(err)
	}
	model := testkit.NewFake()
	s, err := Start(Options{SessionID: "policy-start", Profile: ProfileMemory, Model: model, Store: faults}, manager, "gen")
	policyCode(t, err, product.CodeStorageUnavailable)
	if s != nil || manager.View().LastSeq != 0 || manager.View().ExecutionPolicy.Revision != 0 {
		t.Fatal("failed initial commit exposed policy/runtime")
	}
}

func TestP2PolicyChangeAppendFailurePreservesPolicy(t *testing.T) {
	backend, err := memory.Open("policy-change", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	faults := &p2AttemptFaultStore{Store: backend, failed: make(chan struct{})}
	manager, err := state.NewManager(faults, "policy-change")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Start(Options{SessionID: "policy-change", Profile: ProfileMemory, Model: testkit.NewFake(), Store: faults}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	before := manager.View()
	// Set the fault inside the same mailbox as policy updates.
	if err := s.rt.do(t.Context(), func(*runtime) error { faults.kind = "execution_policy"; return nil }); err != nil {
		t.Fatal(err)
	}
	policyCode(t, s.rt.setExecutionPolicy(t.Context(), 1, agent.ResolvedPolicy{SandboxMode: "read-only"}), product.CodeStorageUnavailable)
	after := manager.View()
	if after.LastSeq != before.LastSeq || after.Cursor != before.Cursor || after.ExecutionPolicy != before.ExecutionPolicy || len(after.Events) != len(before.Events) {
		t.Fatal("failed update became visible")
	}
	reopened, err := state.NewManager(backend, "policy-change")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.View().ExecutionPolicy != before.ExecutionPolicy {
		t.Fatal("failed policy replayed")
	}
}

func TestP2PolicyOpenRestoresPolicyWithoutWidening(t *testing.T) {
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "policy-open", Profile: ProfileMemory, Model: testkit.NewFake(), Policy: &agent.ResolvedPolicy{SandboxMode: "read-only", ApprovalPolicy: "never"}}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	saved := s.rt.manager.View()
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opts.Policy = nil
	s, err = OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if s.rt.manager.View().ExecutionPolicy != saved.ExecutionPolicy || s.rt.manager.View().LastSeq != saved.LastSeq {
		t.Fatal("open changed persisted policy")
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opts.Policy = &agent.ResolvedPolicy{SandboxMode: "danger-full-access"}
	_, err = OpenAgentSession(t.Context(), opts)
	policyCode(t, err, product.CodeStateConflict)
	opts.Policy = &agent.ResolvedPolicy{SandboxMode: "read-only", ApprovalPolicy: "never", Ref: "forged", Revision: 99}
	s, err = OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if s.rt.manager.View().ExecutionPolicy != saved.ExecutionPolicy {
		t.Fatal("caller identity altered policy")
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opts.ReadOnly = true
	opts.Model = nil
	opts.Policy = nil
	s, err = OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if s.rt.manager.View().ExecutionPolicy != saved.ExecutionPolicy || s.rt.manager.View().LastSeq != saved.LastSeq {
		t.Fatal("read-only open changed policy")
	}
	policyCode(t, s.rt.setExecutionPolicy(t.Context(), saved.ExecutionPolicy.Revision, agent.ResolvedPolicy{}), product.CodePermissionDenied)
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestP2PolicyReadOnlyOpenRejectsConflictingDeclaration(t *testing.T) {
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "policy-ro-conflict", Profile: ProfileMemory, Model: testkit.NewFake(), Policy: &agent.ResolvedPolicy{SandboxMode: "read-only"}}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opts.ReadOnly = true
	opts.Model = nil
	opts.Policy = &agent.ResolvedPolicy{SandboxMode: "danger-full-access"}
	opened, err := OpenAgentSession(t.Context(), opts)
	if opened != nil {
		opened.Close(context.Background())
	}
	policyCode(t, err, product.CodeStateConflict)
}

func TestP2PolicyLegacyReadOnlyDoesNotCreatePolicy(t *testing.T) {
	backend, err := memory.Open("policy-legacy", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "policy-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendEvent(t.Context(), "legacy", "", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	before := manager.View().LastSeq
	if err := initializeExecutionPolicy(Options{}, manager); err != nil {
		t.Fatal(err)
	}
	if manager.View().ExecutionPolicy.SandboxMode != "workspace-write" || manager.View().ExecutionPolicy.Revision != 1 || manager.View().LastSeq != before+1 {
		t.Fatal("legacy policy not conservatively initialized")
	}
	backend2, err := memory.Open("policy-readonly", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager2, err := state.NewManager(backend2, "policy-readonly")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager2.AppendEvent(t.Context(), "legacy", "", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	before = manager2.View().LastSeq
	s, err := Start(Options{SessionID: "policy-readonly", Store: backend2, ReadOnly: true}, manager2, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if manager2.View().LastSeq != before || manager2.View().ExecutionPolicy.Revision != 0 {
		t.Fatal("legacy read-only open wrote policy")
	}
}
