package state_test

import (
	"context"
	"encoding/json"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"testing"
	"time"
)

func activityTrace(t *testing.T, limit time.Duration) (*state.Manager, *failingStore, string) {
	t.Helper()
	m, s := fixture(t)
	ctx := context.Background()
	r, err := m.AcceptWithLimits(ctx, agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"hi"}`)}, agent.TargetAgent{Name: "main", Generation: "gen"}, config.Limits{ActivityBudget: limit})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.SetTraceState(ctx, r.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	return m, s, r.TraceID
}

func TestActivitySettlementAndCrashReservations(t *testing.T) {
	ctx := context.Background()
	m, s, id := activityTrace(t, 2300*time.Millisecond)
	before := m.View().LastSeq
	first, err := m.ReserveActivity(ctx, id, "one", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reserved != time.Second || first.Settled != 0 || m.View().LastSeq != before+1 {
		t.Fatalf("first=%+v", first)
	}
	if err = m.SettleActivity(ctx, id, "one", first.Revision, 250*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	got := m.View().Traces[id].Activity
	if got.Settled != 250*time.Millisecond || got.Reserved != 0 || got.Uncertain != 0 {
		t.Fatalf("settled=%+v", got)
	}
	// No clock timestamp exists to bill the hours spent paused.
	before = m.View().LastSeq
	reopened, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.View().Traces[id].Activity != got || reopened.View().LastSeq != before {
		t.Fatal("reopen changed safe settlement")
	}
	second, err := reopened.ReserveActivity(ctx, id, "two", got.Revision, 0)
	if err != nil {
		t.Fatal(err)
	}
	if second.Reserved != time.Second {
		t.Fatal(second)
	}
	crash, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	lost := crash.View().Traces[id].Activity
	if lost.Uncertain != time.Second || lost.Reserved != 0 || lost.Settled != 250*time.Millisecond {
		t.Fatalf("crash=%+v", lost)
	}
	third, err := crash.ReserveActivity(ctx, id, "three", lost.Revision, 0)
	if err != nil {
		t.Fatal(err)
	}
	again, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	final, err := again.ReserveActivity(ctx, id, "four", third.Revision, 0)
	if err != nil {
		t.Fatal(err)
	}
	if final.Reserved != 50*time.Millisecond || final.Uncertain != 2*time.Second {
		t.Fatalf("crash refunded budget: %+v", final)
	}
	last, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	_, err = last.ReserveActivity(ctx, id, "five", final.Revision, 0)
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeBudgetExhausted {
		t.Fatalf("err=%v", err)
	}
}

func TestActivityRemaining300msAndCountersMerge(t *testing.T) {
	m, _, id := activityTrace(t, 300*time.Millisecond)
	ctx := context.Background()
	r, err := m.ReserveActivity(ctx, id, "segment", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Reserved != 300*time.Millisecond {
		t.Fatal(r)
	}
	if err = m.SaveTraceBudget(ctx, id, agent.Usage{LogicalModelCalls: 1, ModelCallID: "model"}); err != nil {
		t.Fatal(err)
	}
	if m.View().Traces[id].Activity != r {
		t.Fatal("model usage overwrote activity")
	}
	if err = m.SettleActivity(ctx, id, "segment", r.Revision, 250*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if m.View().Traces[id].Usage.LogicalModelCalls != 1 {
		t.Fatal("activity overwrote model usage")
	}
	before := m.View().LastSeq
	if err = m.SettleActivity(ctx, id, "segment", r.Revision, 0); err == nil || m.View().LastSeq != before {
		t.Fatal("stale settlement refunded activity")
	}
}

func TestActivityLegacyUnstartedCanInitialize(t *testing.T) {
	_, s := fixture(t)
	_, err := s.Append(context.Background(), "session", store.ExpectedCommit{}, store.Commit{RecordType: "commit", Version: 1, CommitID: "legacy", CommitSeq: 1, ControlRecords: []store.Record{{Type: "trace", ID: "legacy", Version: 1, Payload: json.RawMessage(`{"id":"legacy","state":"queued","started":false,"limits":{"ActivityBudget":1000000000}}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.SetTraceState(context.Background(), "legacy", "running", false); err != nil {
		t.Fatal(err)
	}
	a, err := m.ReserveActivity(context.Background(), "legacy", "first", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Known || a.Unknown || a.Reserved != time.Second || m.View().LastSeq != 3 {
		t.Fatalf("activity=%+v commits=%d", a, m.View().LastSeq)
	}
}

func TestActivityInitialCommitFailureHasNoReservation(t *testing.T) {
	m, s, id := activityTrace(t, time.Second)
	before := m.View().LastSeq
	s.fail = true
	if _, err := m.ReserveActivity(context.Background(), id, "one", 0, 0); err == nil {
		t.Fatal("expected append failure")
	}
	a := m.View().Traces[id].Activity
	if a.Reserved != 0 || a.Revision != 0 || a.Settled != 0 || m.View().LastSeq != before {
		t.Fatal("failed append published occupancy")
	}
}

func TestActivityLegacyStartedIsUnknown(t *testing.T) {
	_, s := fixture(t)
	_, err := s.Append(context.Background(), "session", store.ExpectedCommit{}, store.Commit{RecordType: "commit", Version: 1, CommitID: "legacy", CommitSeq: 1, ControlRecords: []store.Record{{Type: "trace", ID: "legacy", Version: 1, Payload: json.RawMessage(`{"id":"legacy","state":"running","started":true}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m.View().Traces["legacy"])
	var got struct {
		Activity struct {
			Unknown bool `json:"unknown"`
		} `json:"activity"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Activity.Unknown {
		t.Fatal("legacy started trace was interpreted as precisely zero activity")
	}
	_, reserveErr := m.ReserveActivity(context.Background(), "legacy", "new", 0, 0)
	pe, ok := product.AsError(reserveErr)
	if !ok || pe.Code != product.CodeReconciliationRequired {
		t.Fatalf("legacy activity was granted: %v", reserveErr)
	}
	if m.View().LastSeq != 1 {
		t.Fatal("browsing wrote a recovery commit")
	}
}
