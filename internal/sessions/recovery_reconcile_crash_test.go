package sessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

// These are four real process-death boundaries, not failed-Append simulations.
// result_before is reached only after Query returned evidence to Reconcile;
// result_after is after JSONL Append succeeded, before in-memory hold release.
// Late-result races remain covered separately by reconciliation_followup_test.go.
func TestRecoveryReconcileCrash(t *testing.T) {
	if os.Getenv("SEASPRAK_RECONCILE_CRASH_CHILD") == "1" {
		recoveryReconcileChild(t)
		return
	}
	for _, window := range []string{"accepted", "result"} {
		for _, phase := range []string{"before", "after"} {
			t.Run(window+"_"+phase, func(t *testing.T) {
				ws, root := t.TempDir(), t.TempDir()
				model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "execute", Name: "execute", Arguments: `{"argv":["echo","hi"],"cwd":"workspace"}`}}}, testkit.Step{Text: "done"})
				process := &diskUnknownProcess{}
				opts := recoveryReconcileOptions(t, ws, root, model, process)
				first, err := CreateAgentSession(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = first.Close(context.Background()) })
				receipt := submitOutput(t, first)
				waitResumeCondition(t, func() bool {
					tr := first.rt.manager.View().Traces[receipt.TraceID]
					return tr.ExecutionStopped && terminal(tr.State)
				})
				before := first.rt.manager.View()
				call := onlyControlledCall(t, first)
				original := latestReconcileObservation(before, call.Call.CallID)
				if process.calls.Load() != 1 || model.Calls() != 1 || call.Observation == nil || call.Observation.SideEffect != "unknown" || original.Version == 0 {
					t.Fatalf("fixture counts/state: process=%d model=%d observation=%+v", process.calls.Load(), model.Calls(), original)
				}
				closeCrashed(t, first)
				rep := recoveryReconcileKill(t, ws, root, window, phase)
				wantQueries := int32(0)
				if window == "result" {
					wantQueries = 1
				}
				if rep.Queries != wantQueries || rep.ModelCalls != 0 || rep.ToolCalls != 0 {
					t.Fatalf("child calls: query=%d model=%d tool=%d", rep.Queries, rep.ModelCalls, rep.ToolCalls)
				}
				crashed, err := loadCrashJournal(root, opts.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				expected := append([]store.Commit(nil), rep.Prefix...)
				if phase == "after" {
					expected = append(expected, rep.Target)
				}
				if !reflect.DeepEqual(crashed.Commits, expected) {
					t.Fatal("killed journal differs from acknowledged prefix/target")
				}
				committed := window == "result" && phase == "after"
				for reopen := 0; reopen < 2; reopen++ {
					freshModel, freshProcess := testkit.NewFake(testkit.Step{Text: "must not execute"}), &diskUnknownProcess{}
					var queries atomic.Int32
					openOpts := recoveryReconcileOptions(t, ws, root, freshModel, freshProcess)
					openOpts.ReconcileQueries = map[string]ReconcileQuery{"no-start": ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
						queries.Add(1)
						return ReconcileEvidence{}, errors.New("Open must not query")
					})}
					opened, err := OpenAgentSession(t.Context(), openOpts)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = opened.Close(context.Background()) })
					if _, ok := opened.rt.opts.Store.(*jsonl.Store); !ok {
						t.Fatal("Open did not use real JSONL")
					}
					view := opened.rt.manager.View()
					if !reflect.DeepEqual(view.Calls, before.Calls) || !reflect.DeepEqual(view.Messages, before.Messages) || !reflect.DeepEqual(view.Traces, before.Traces) || view.Budget != before.Budget {
						t.Fatal("Open/reconciliation changed original history, terminal trace, or budget")
					}
					if !reflect.DeepEqual(view.Observations[original.ID], original) {
						t.Fatal("original unknown observation overwritten")
					}
					if openOpts.ResourceScheduler.HasHold(tools.ResourceHoldID(opts.SessionID, call.Call.CallID)) == committed || view.HasUnresolvedEffects() == committed {
						t.Fatal("hold/unknown recovery disagrees with durable release")
					}
					latest := latestReconcileObservation(view, call.Call.CallID)
					wantResults := 0
					if committed {
						wantResults = 1
						if latest.Version != original.Version+1 || latest.PreviousID != original.ID || latest.Observation.SideEffect != "none" || latest.Observation.Executed {
							t.Fatal("committed known observation not recovered")
						}
					} else if !reflect.DeepEqual(latest, original) {
						t.Fatal("uncommitted query published an observation")
					}
					if len(view.ResourceHoldReleases) != wantResults || len(view.Reconciliations) != wantResults {
						t.Fatal("partial reconciliation/release recovered")
					}
					wantOps := 1
					if window == "accepted" && phase == "before" {
						wantOps = 0
					}
					if len(view.Operations) != wantOps {
						t.Fatalf("operations=%d want=%d", len(view.Operations), wantOps)
					}
					for _, op := range view.Operations {
						wantState := "accepted"
						if window == "result" {
							wantState = "running"
						}
						if committed {
							wantState = "completed"
						}
						if op.State != wantState {
							t.Fatalf("operation state=%s want=%s", op.State, wantState)
						}
					}
					if _, err := opened.Snapshot(t.Context()); err != nil {
						t.Fatal(err)
					}
					closeCrashed(t, opened)
					if freshModel.Calls() != 0 || freshProcess.calls.Load() != 0 || queries.Load() != 0 {
						t.Fatal("Open/Snapshot/Close reran model, tool, or query")
					}
					journal, err := loadCrashJournal(root, opts.SessionID)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(journal.Commits, crashed.Commits) || countSettled(journal) != 1 {
						t.Fatal("browsing wrote journal or revived terminal task")
					}
				}
				if window == "result" {
					recoveryReconcileAssertTransaction(t, rep.Target, original)
				}
				t.Logf("Kill/Wait %s_%s: original model/tool=1/1; child model/tool/query=0/0/%d; two disk Opens=0/0/0; release=%t", window, phase, rep.Queries, committed)
			})
		}
	}
}

func recoveryReconcileOptions(t *testing.T, ws, root string, model *testkit.FakeModel, process *diskUnknownProcess) Options {
	t.Helper()
	return Options{SessionID: "reconcile-crash", Workspace: ws, StateRoot: root, Profile: ProfileMemory, Principal: "operator", Model: model, Tools: []tools.Definition{builtinDefinitionForSession(t, "execute")}, Operations: tools.Operations{Process: process}, ResourceScheduler: tools.NewResourceScheduler()}
}

type recoveryReconcileReport struct {
	crashReport
	Queries int32 `json:"queries"`
}

type recoveryReconcileStore struct {
	store.Store
	window, phase string
	model         *testkit.FakeModel
	process       *diskUnknownProcess
	queries       *atomic.Int32
}

func (s *recoveryReconcileStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	match := false
	for _, r := range c.ControlRecords {
		if s.window == "result" && r.Type == "reconciliation" {
			match = true
		}
		if s.window == "accepted" && r.Type == "operation" {
			var op state.Operation
			if err := json.Unmarshal(r.Payload, &op); err != nil {
				return store.CommitReceipt{}, err
			}
			if op.Kind == "reconcile" && op.State == "accepted" {
				match = true
			}
		}
	}
	if !match {
		return s.Store.Append(ctx, id, expected, c)
	}
	loaded, err := s.Store.Load(ctx, id)
	if err != nil {
		return store.CommitReceipt{}, err
	}
	hold := func() {
		report := recoveryReconcileReport{crashReport: crashReport{Window: s.window, Phase: s.phase, ModelCalls: s.model.Calls(), ToolCalls: int(s.process.calls.Load()), Target: c, Prefix: loaded.Commits}, Queries: s.queries.Load()}
		raw, err := json.Marshal(report)
		if err != nil {
			panic(err)
		}
		if _, err := fmt.Fprintln(os.Stdout, crashReadyPrefix+string(raw)); err != nil {
			panic(err)
		}
		_ = os.Stdout.Sync()
		select {}
	}
	if s.phase == "before" {
		hold()
	}
	receipt, err := s.Store.Append(ctx, id, expected, c)
	if err == nil && s.phase == "after" {
		hold()
	}
	return receipt, err
}

func recoveryReconcileChild(t *testing.T) {
	model, process := testkit.NewFake(testkit.Step{Text: "must not execute"}), &diskUnknownProcess{}
	opts := recoveryReconcileOptions(t, os.Getenv("SEASPRAK_RECONCILE_CRASH_WS"), os.Getenv("SEASPRAK_RECONCILE_CRASH_ROOT"), model, process)
	var queries atomic.Int32
	opts.ReconcileQueries = map[string]ReconcileQuery{"no-start": ReconcileQueryFunc(func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error) {
		queries.Add(1)
		return ReconcileEvidence{TrustedNoStart: true, EvidenceSource: "controlled-admission-ledger", EvidenceRefs: []string{"admission-not-started"}}, nil
	})}
	s, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	// Replace only the persistence port in the mailbox, while idle. All records
	// still originate from public Reconcile; no synthetic records are appended.
	if err := s.rt.do(t.Context(), func(rt *runtime) error {
		wrapped := &recoveryReconcileStore{Store: rt.opts.Store, window: os.Getenv("SEASPRAK_RECONCILE_CRASH_WINDOW"), phase: os.Getenv("SEASPRAK_RECONCILE_CRASH_PHASE"), model: model, process: process, queries: &queries}
		manager, err := state.NewManager(wrapped, opts.SessionID)
		if err != nil {
			return err
		}
		rt.manager = manager
		rt.opts.Store = wrapped
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	call := onlyControlledCall(t, s)
	view := s.rt.manager.View()
	observation := latestReconcileObservation(view, call.Call.CallID)
	_, err = s.Reconcile(t.Context(), ReconcileCommand{TraceID: call.Scope.TraceID, InvocationID: call.Scope.InvocationID, CallID: call.Call.CallID, ObservationID: observation.ID, ObservationVersion: observation.Version, ExpectedRevision: view.LastSeq, QueryID: "no-start", IdempotencyKey: "crash-reconcile"})
	t.Fatalf("Reconcile returned instead of reaching barrier: %v", err)
}

func recoveryReconcileKill(t *testing.T, ws, root, window, phase string) recoveryReconcileReport {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryReconcileCrash$", "-test.count=1", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "SEASPRAK_RECONCILE_CRASH_CHILD=1", "SEASPRAK_RECONCILE_CRASH_WS="+ws, "SEASPRAK_RECONCILE_CRASH_ROOT="+root, "SEASPRAK_RECONCILE_CRASH_WINDOW="+window, "SEASPRAK_RECONCILE_CRASH_PHASE="+phase)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr, output bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan recoveryReconcileReport, 1)
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 4<<20)
		for scanner.Scan() {
			line := scanner.Text()
			output.WriteString(line + "\n")
			if strings.HasPrefix(line, crashReadyPrefix) {
				var rep recoveryReconcileReport
				if json.Unmarshal([]byte(strings.TrimPrefix(line, crashReadyPrefix)), &rep) == nil {
					select {
					case ready <- rep:
					default:
					}
				}
			}
		}
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("Kill: %v", err)
		}
		select {
		case <-scanned:
		case <-time.After(15 * time.Second):
			_ = stdout.Close()
			t.Error("stdout did not close after Kill")
			<-scanned
		}
		if err := cmd.Wait(); err == nil {
			t.Error("child exited successfully instead of being killed")
		}
	}
	defer stop()
	var rep recoveryReconcileReport
	select {
	case rep = <-ready:
		stop()
	case <-scanned:
		stop()
		t.Fatalf("child exited without barrier: stdout=%s stderr=%s", output.String(), stderr.String())
	case <-time.After(40 * time.Second):
		stop()
		t.Fatalf("barrier timeout: stdout=%s stderr=%s", output.String(), stderr.String())
	}
	if rep.Window != window || rep.Phase != phase {
		t.Fatal("wrong crash barrier")
	}
	return rep
}

func recoveryReconcileAssertTransaction(t *testing.T, c store.Commit, old state.ObservationRevision) {
	t.Helper()
	counts := map[string]int{}
	var release state.ResourceHoldRelease
	var result state.Reconciliation
	var observation state.ObservationRevision
	var operation state.Operation
	for _, r := range c.ControlRecords {
		counts[r.Type]++
		var target any
		switch r.Type {
		case "resource_hold_release":
			target = &release
		case "reconciliation":
			target = &result
		case "observation_revision":
			target = &observation
		case "operation":
			target = &operation
		}
		if target != nil {
			if err := json.Unmarshal(r.Payload, target); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, kind := range []string{"resource_hold_release", "reconciliation", "observation_revision", "operation"} {
		if counts[kind] != 1 {
			t.Fatalf("transaction %s count=%d", kind, counts[kind])
		}
	}
	if release.ReconciliationID != result.ID || release.ObservationID != observation.ID || result.NewObservationID != observation.ID || result.OperationID != operation.Receipt.OperationID || operation.State != "completed" || operation.ResultRef != result.ID || result.ObservationID != old.ID || observation.PreviousID != old.ID || observation.Version != old.Version+1 || !result.TrustedNoStart {
		t.Fatal("release/conclusion/observation/operation identities are not one atomic chain")
	}
}
