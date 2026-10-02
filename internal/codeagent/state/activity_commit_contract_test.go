package state

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// Use the interface-level journal chain, not a concrete backend or benchmark fixture.
type activityContractStore struct {
	store.Store
	chain   *store.Chain
	calls   int
	before  func()
	failure error
	lostAck bool
}

func (s *activityContractStore) Load(context.Context, string) (store.StoredSession, error) {
	return s.chain.Session(store.Header{}, false), nil
}

func (s *activityContractStore) Append(_ context.Context, _ string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	s.calls++
	if s.before != nil {
		s.before()
	}
	if s.failure != nil && !s.lostAck {
		return store.CommitReceipt{}, s.failure
	}
	sealed, receipt, duplicate, err := s.chain.Prepare(expected, c)
	if err != nil || duplicate {
		return receipt, err
	}
	if err := s.chain.Apply(sealed); err != nil {
		return store.CommitReceipt{}, err
	}
	if s.failure != nil {
		return store.CommitReceipt{}, s.failure
	}
	return receipt, nil
}

func activityContractManager(t *testing.T) (*Manager, *activityContractStore) {
	t.Helper()
	s := &activityContractStore{chain: store.NewChain()}
	m, err := NewManager(s, "activity-contract")
	if err != nil {
		t.Fatal(err)
	}
	tr := TraceState{ID: "trace", State: "running", Started: true, Generation: "gen", Limits: config.DefaultLimits(), Activity: ActivityBudget{Known: true}, HoldOnStop: []string{"queued"}}
	m.mu.Lock()
	_, err = m.commit(t.Context(), []store.Record{record("trace", tr.ID, tr)}, nil, nil)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReserveActivity(t.Context(), "trace", "execution", 0, 0); err != nil {
		t.Fatal(err)
	}
	return m, s
}

type activityHistoryProbe struct {
	calls  atomic.Int32
	onRead func()
}

func (p *activityHistoryProbe) MarshalJSON() ([]byte, error) {
	p.calls.Add(1)
	if p.onRead != nil {
		p.onRead()
	}
	return []byte(`"unrelated-history"`), nil
}

// This is a structural cost contract, not a timing threshold or a replacement
// for the real-clock stress tests. Both operations must avoid serializing history.
func TestActivityCommitDoesNotSerializeUnrelatedHistory(t *testing.T) {
	for _, operation := range []string{"renew", "settle"} {
		t.Run(operation, func(t *testing.T) {
			m, s := activityContractManager(t)
			probe := &activityHistoryProbe{onRead: func() {
				if m.mu.TryLock() {
					m.mu.Unlock()
					t.Error("history serialization unexpectedly outside Manager lock")
				}
			}}
			// Install a valid JSON-valued diagnostic in unrelated history after
			// setup; no production test hooks or clock changes are needed.
			m.view.Messages = []agent.AgentMessage{{ID: "history", Kind: agent.KindAssistant, Status: agent.StatusComplete, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, Extra: map[string]any{"probe": probe}}}}
			before := s.calls
			var err error
			if operation == "renew" {
				_, err = m.ReserveActivity(t.Context(), "trace", "execution", 1, 500*time.Millisecond)
			} else {
				err = m.SettleActivity(t.Context(), "trace", "execution", 1, 500*time.Millisecond)
			}
			if err != nil || s.calls != before+1 {
				t.Fatalf("activity commit err=%v append calls=%d want=%d", err, s.calls, before+1)
			}
			if got := probe.calls.Load(); got != 0 {
				t.Fatalf("activity %s serialized unrelated history %d times while holding Manager.mu; want 0", operation, got)
			}
		})
	}
}

func TestActivityCommitPublicationContract(t *testing.T) {
	for _, operation := range []string{"renew", "settle"} {
		for _, mode := range []string{"success", "reject", "lost_ack"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				m, s := activityContractManager(t)
				before, owner := m.View(), m.view
				calls := s.calls
				// This callback is inside Append, on the committing goroutine,
				// while Manager.mu is owned. Never recursively call View here.
				s.before = func() {
					if m.view != owner || !reflect.DeepEqual(*m.view, before) {
						t.Error("candidate became visible before Append acknowledged")
					}
					if m.mu.TryLock() {
						m.mu.Unlock()
						t.Error("Append lost Manager lock ownership")
					}
				}
				if mode != "success" {
					s.failure = errors.New("injected activity append failure")
					s.lostAck = mode == "lost_ack"
				}
				var err error
				var reserved ActivityBudget
				if operation == "renew" {
					reserved, err = m.ReserveActivity(t.Context(), "trace", "execution", 1, 500*time.Millisecond)
				} else {
					err = m.SettleActivity(t.Context(), "trace", "execution", 1, 500*time.Millisecond)
				}
				if err != nil && reserved != (ActivityBudget{}) {
					t.Fatalf("failed renewal returned an unpublished reservation: %+v", reserved)
				}
				if s.calls != calls+1 || !reflect.DeepEqual(*owner, before) {
					t.Fatal("commit changed old owner or append count")
				}
				after := m.View()
				if mode == "success" {
					if err != nil || m.Fault() != nil || after.LastSeq != before.LastSeq+1 || after.Traces["trace"].Activity.Revision != 2 || after.Traces["trace"].Activity.Settled != 500*time.Millisecond || after.Cursor != before.Cursor || len(after.Events) != len(before.Events) {
						t.Fatalf("successful publication err=%v activity=%+v", err, after.Traces["trace"].Activity)
					}
				} else {
					if !errors.Is(err, s.failure) || !reflect.DeepEqual(after, before) {
						t.Fatalf("failed append published state: err=%v activity=%+v", err, after.Traces["trace"].Activity)
					}
					pe, ok := product.AsError(m.Fault())
					if !ok || pe.Code != product.CodeStorageUnavailable {
						t.Fatalf("missing write fault: %v", m.Fault())
					}
					_, retryErr := m.ReserveActivity(t.Context(), "trace", "execution", 1, 0)
					if retryErr == nil || s.calls != calls+1 {
						t.Fatal("faulted manager retried Append")
					}
				}
				reopened, err := NewManager(s, "activity-contract")
				if err != nil {
					t.Fatal(err)
				}
				got := reopened.View().Traces["trace"]
				want := ActivityBudget{Known: true, Revision: 2, Settled: 500 * time.Millisecond}
				if operation == "renew" {
					want.Uncertain = time.Second
				}
				if mode == "reject" {
					want = ActivityBudget{Known: true, Revision: 1, Uncertain: time.Second}
				}
				if got.Activity != want || got.ExecutionStopped {
					t.Fatalf("replay occupancy=%+v want=%+v stopped=%v", got.Activity, want, got.ExecutionStopped)
				}
			})
		}
	}
}

func TestActivityCandidateValidationPrecedesAppend(t *testing.T) {
	m, s := activityContractManager(t)
	// Deliberately corrupt the in-memory fixture to exercise applyCommit's
	// final cross-trace invariant, after its trace record has been applied.
	m.view.Traces["other"] = &TraceState{ID: "other", State: "running", Started: true}
	before, calls := m.View(), s.calls
	_, err := m.ReserveActivity(t.Context(), "trace", "execution", 1, 500*time.Millisecond)
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeIncompatibleVersion || s.calls != calls || m.Fault() != nil || !reflect.DeepEqual(m.View(), before) {
		t.Fatalf("invalid candidate was appended or published: err=%v calls=%d want=%d", err, s.calls, calls)
	}
}
