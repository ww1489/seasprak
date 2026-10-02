package state

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

func installActivityHistoryProbe(m *Manager) *activityHistoryProbe {
	probe := &activityHistoryProbe{}
	m.view.Messages = []agent.AgentMessage{{ID: "history", Kind: agent.KindAssistant, Status: agent.StatusComplete, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, Extra: map[string]any{"probe": probe}}}}
	return probe
}

func TestActivityNarrowReadsOwnValuesAndAvoidHistory(t *testing.T) {
	m, s := activityContractManager(t)
	reader, ok := any(m).(interface {
		ActivityState(string) (time.Duration, ActivityBudget, bool)
		WriteStatus() error
	})
	if !ok {
		t.Fatal("Manager lacks value-only activity/write-status reads")
	}
	probe := installActivityHistoryProbe(m)
	calls := s.calls
	limit, a, found := reader.ActivityState("trace")
	if !found || limit != m.view.Traces["trace"].Limits.ActivityBudget || a != m.view.Traces["trace"].Activity {
		t.Fatalf("projection limit=%v activity=%+v found=%v", limit, a, found)
	}
	a.Reserved = 0
	a.ExecutionID = "caller"
	_, again, _ := reader.ActivityState("trace")
	if again.Reserved != time.Second || again.ExecutionID != "execution" {
		t.Fatal("activity projection aliases caller data")
	}
	if limit, a, found := reader.ActivityState("missing"); found || limit != 0 || a != (ActivityBudget{}) {
		t.Fatal("missing trace returned a reservation")
	}
	fault := errors.New("original store fault")
	for _, tc := range []struct {
		repair bool
		fault  error
	}{{}, {repair: true}, {fault: fault}, {repair: true, fault: fault}} {
		m.view.RepairRequired, m.fault = tc.repair, tc.fault
		err := reader.WriteStatus()
		if tc.fault != nil {
			if err != fault {
				t.Fatalf("fault lost priority/identity: %v", err)
			}
		} else if tc.repair {
			pe, ok := product.AsError(err)
			if !ok || pe.Code != product.CodeStorageUnavailable || pe.Message != "journal requires repair" {
				t.Fatalf("repair error changed: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if probe.calls.Load() != 0 || s.calls != calls {
		t.Fatalf("read serialized history=%d or appended=%d", probe.calls.Load(), s.calls-calls)
	}
}

func TestActivityCandidateAdmissionRejectsMixedChanges(t *testing.T) {
	for _, kind := range []string{"state", "usage", "limits", "generation", "hold_slice", "stopped", "missing", "identity", "version", "parent", "extra_control", "entry", "event", "no_control", "non_trace", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			m, s := activityContractManager(t)
			writer, ok := any(m).(interface {
				commitWithCandidate(context.Context, []store.Record, []store.Record, []agent.Event, bool) (store.CommitReceipt, error)
			})
			if !ok {
				t.Fatal("Manager lacks guarded activity candidate path")
			}
			next := clone(*m.view.Traces["trace"])
			next.Activity.Revision++
			switch kind {
			case "state":
				next.State = "paused"
			case "usage":
				next.Usage.ToolExecutions++
			case "limits":
				next.Limits.ActivityBudget++
			case "generation":
				next.Generation = "changed"
			case "hold_slice":
				next.HoldOnStop[0] = "changed"
			case "stopped":
				next.ExecutionStopped = true
			case "missing":
				next.ID = "missing"
			case "identity":
				next.ID = "different"
			}
			r := record("trace", "trace", next)
			switch kind {
			case "missing":
				r.ID = "missing"
			case "version":
				r.Version++
			case "parent":
				r.ParentID = "unexpected"
			case "non_trace":
				r.Type = "queue_hold"
			case "malformed":
				r.Payload = []byte(`{`)
			}
			controls := []store.Record{r}
			var entries []store.Record
			var events []agent.Event
			switch kind {
			case "no_control":
				controls = nil
			case "extra_control":
				controls = append(controls, record("budget", "budget", agent.Usage{}))
			case "entry":
				entries = []store.Record{record("message", "message", agent.AgentMessage{})}
			case "event":
				events = []agent.Event{m.event("diagnostic", "trace", "", nil)}
			}
			before, owner, calls := m.View(), m.view, s.calls
			m.mu.Lock()
			_, err := writer.commitWithCandidate(t.Context(), controls, entries, events, true)
			m.mu.Unlock()
			pe, ok := product.AsError(err)
			if !ok || pe.Code != product.CodeStateConflict || m.view != owner || !reflect.DeepEqual(m.View(), before) || s.calls != calls || m.Fault() != nil {
				t.Fatalf("mixed update escaped guard: err=%v append=%d", err, s.calls-calls)
			}
		})
	}
}

func TestActivityCandidateRejectsBranchUpdates(t *testing.T) {
	m, _ := activityContractManager(t)
	next := *m.view.Traces["trace"]
	next.Activity.Revision++
	c := store.Commit{ControlRecords: []store.Record{record("trace", "trace", next)}, BranchUpdates: []store.BranchUpdate{{BranchID: "other"}}}
	before := m.View()
	m.mu.Lock()
	_, err := m.activityCandidate(c)
	m.mu.Unlock()
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeStateConflict || !reflect.DeepEqual(m.View(), before) {
		t.Fatalf("branch update entered shallow candidate: %v", err)
	}
}

func TestActivityOptimizationLeavesGenericCommitCloning(t *testing.T) {
	m, _ := activityContractManager(t)
	probe := installActivityHistoryProbe(m)
	next := *m.view.Traces["trace"]
	next.Activity.Revision++
	m.mu.Lock()
	_, err := m.commit(t.Context(), []store.Record{record("trace", "trace", next)}, nil, nil)
	m.mu.Unlock()
	if err != nil || probe.calls.Load() != 1 {
		t.Fatalf("generic commit changed clone contract: err=%v history_reads=%d", err, probe.calls.Load())
	}
}

func TestActivityCommitOwnsTargetTraceAndNestedSlice(t *testing.T) {
	m, _ := activityContractManager(t)
	owner := m.view
	before := m.View()
	if _, err := m.ReserveActivity(t.Context(), "trace", "execution", 1, 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	m.view.Traces["trace"].HoldOnStop[0] = "changed"
	m.view.Traces["added"] = &TraceState{ID: "added"}
	if !reflect.DeepEqual(*owner, before) {
		t.Fatal("activity candidate shares its mutable target/map with old View")
	}
}
