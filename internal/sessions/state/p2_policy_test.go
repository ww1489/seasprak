package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

// Independent representation asserts the canonical reference input, excluding Ref.
func policyFixture(revision uint64, mode, approval string) agent.ResolvedPolicy {
	body, _ := json.Marshal(struct {
		Revision       uint64 `json:"revision"`
		SandboxMode    string `json:"sandboxMode"`
		ApprovalPolicy string `json:"approvalPolicy"`
		Auto           bool   `json:"auto"`
	}{revision, mode, approval, false})
	sum := sha256.Sum256(body)
	return agent.ResolvedPolicy{Ref: hex.EncodeToString(sum[:]), Revision: revision, SandboxMode: mode, ApprovalPolicy: approval}
}
func TestP2PolicyRecordReplay(t *testing.T) {
	for _, bad := range []string{"", "ref", "revision", "mode", "auto"} {
		t.Run(bad, func(t *testing.T) {
			backend, err := memory.Open("policy", store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			p := policyFixture(1, "workspace-write", "ask")
			switch bad {
			case "ref":
				p.Ref = "forged"
			case "revision":
				p = policyFixture(2, "workspace-write", "ask")
			case "mode":
				p = policyFixture(1, "invalid", "ask")
			case "auto":
				p.Auto = true
			}
			raw, _ := json.Marshal(p)
			_, err = backend.Append(t.Context(), "policy", store.ExpectedCommit{}, store.Commit{RecordType: "commit", Version: 1, CommitID: "one", CommitSeq: 1, ControlRecords: []store.Record{{Type: "execution_policy", Version: 1, ID: "current", Payload: raw}}})
			if err != nil {
				t.Fatal(err)
			}
			m, err := NewManager(backend, "policy")
			if bad != "" {
				if err == nil {
					t.Fatal("invalid policy replayed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(m.View())
			var v map[string]json.RawMessage
			_ = json.Unmarshal(encoded, &v)
			var got agent.ResolvedPolicy
			_ = json.Unmarshal(v["ExecutionPolicy"], &got)
			if got != p {
				t.Fatalf("got=%+v want=%+v", got, p)
			}
		})
	}
}

type policyWriter interface {
	SetExecutionPolicy(context.Context, uint64, agent.ResolvedPolicy) error
}
type policyFaultStore struct {
	store.Store
	fail bool
}

func (s *policyFaultStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	if s.fail {
		return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "policy append rejected")
	}
	return s.Store.Append(ctx, id, expected, c)
}
func TestP2PolicyCASAndAppendVisibility(t *testing.T) {
	backend, err := memory.Open("policy", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	faults := &policyFaultStore{Store: backend}
	m, err := NewManager(faults, "policy")
	if err != nil {
		t.Fatal(err)
	}
	w, ok := any(m).(policyWriter)
	if !ok {
		t.Fatal("manager cannot commit execution policy")
	}
	if err := w.SetExecutionPolicy(t.Context(), 0, agent.ResolvedPolicy{Ref: "forged", Revision: 99}); err != nil {
		t.Fatal(err)
	}
	gotPolicy := func() agent.ResolvedPolicy {
		raw, _ := json.Marshal(m.View())
		var v map[string]json.RawMessage
		_ = json.Unmarshal(raw, &v)
		var p agent.ResolvedPolicy
		_ = json.Unmarshal(v["ExecutionPolicy"], &p)
		return p
	}
	if gotPolicy() != policyFixture(1, "workspace-write", "ask") {
		t.Fatalf("policy=%+v", gotPolicy())
	}
	before := m.View().LastSeq
	err = w.SetExecutionPolicy(t.Context(), 0, agent.ResolvedPolicy{SandboxMode: "read-only"})
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeStateConflict || m.View().LastSeq != before {
		t.Fatalf("stale CAS=%v", err)
	}
	if err := w.SetExecutionPolicy(t.Context(), 1, agent.ResolvedPolicy{SandboxMode: "read-only", ApprovalPolicy: "never"}); err != nil {
		t.Fatal(err)
	}
	if gotPolicy() != policyFixture(2, "read-only", "never") {
		t.Fatal("incorrect next policy")
	}
	stored, err := backend.Load(t.Context(), "policy")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range stored.Commits {
		if len(c.ControlRecords) != 1 || len(c.Events) != 1 || c.Events[0].Type != "security.policy_changed" {
			t.Fatal("policy and event not atomic")
		}
	}
	before = m.View().LastSeq
	old := gotPolicy()
	faults.fail = true
	err = w.SetExecutionPolicy(t.Context(), 2, agent.ResolvedPolicy{SandboxMode: "danger-full-access"})
	pe, ok = product.AsError(err)
	if !ok || pe.Code != product.CodeStorageUnavailable || m.View().LastSeq != before || gotPolicy() != old {
		t.Fatal("failed append exposed policy")
	}
	reopened, err := NewManager(backend, "policy")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.View().LastSeq != before {
		t.Fatal("rejected policy survived replay")
	}
}
