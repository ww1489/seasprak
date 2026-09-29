package sessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

const recoveryApprovalReady = "SEASPRAK_APPROVAL_CRASH_READY "

type recoveryApprovalReport struct {
	Before        state.View
	Response      InteractionResponse
	TraceID       string
	ModelCalls    int
	Target        store.Commit
	Durable       state.View
	Prefix        []store.Commit
	ToolCalls     int
	ReturnedCalls int
}

// Embedding keeps the existing synthetic capability identity and authorization
// checks. The durable counter detects execution across the process boundary.
type recoveryApprovalProcess struct {
	commandProcessProbe
	effect  string
	barrier *recoveryApprovalStore
}

func (p *recoveryApprovalProcess) Execute(ctx context.Context, req agent.AuthorizedProcess, sink agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := bumpEffect(p.effect); err != nil {
		return agent.ProcessObservation{}, err
	}
	if p.barrier != nil && p.barrier.window == "execute_entered" {
		p.barrier.hold()
	}
	obs, err := p.commandProcessProbe.Execute(ctx, req, sink)
	if err == nil {
		if countErr := bumpEffect(p.effect + "-returned"); countErr != nil {
			return agent.ProcessObservation{}, countErr
		}
	}
	if p.barrier != nil && p.barrier.window == "execute_returned" {
		p.barrier.hold()
	}
	return obs, err
}

func recoveryApprovalOptions(t *testing.T, root string) (Options, *testkit.FakeModel, *recoveryApprovalProcess) {
	t.Helper()
	model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "execute", Name: "execute", Arguments: `{"argv":["echo","hi"],"cwd":"workspace"}`}}}, testkit.Step{Text: "done"})
	process := &recoveryApprovalProcess{effect: filepath.Join(root, "effect")}
	def := builtinDefinitionForSession(t, "execute")
	def.Execution.RequestedGrantRef = "requires-approval"
	// Same real Eino/controlled-tool setup as capabilityApprovalFixture.
	opts := Options{SessionID: "approval-crash", Workspace: filepath.Join(root, "workspace"), StateRoot: filepath.Join(root, "state"), Profile: ProfileMemory, Principal: "host", GenerationFingerprint: "capability-model-approval", Model: versionedPauseModel{model}, Tools: []tools.Definition{def}, Operations: tools.Operations{Process: process}, ResourceScheduler: tools.NewResourceScheduler()}
	return opts, model, process
}

type recoveryApprovalStore struct {
	store.Store
	store.CheckpointBlobs
	window  string
	report  recoveryApprovalReport
	model   *testkit.FakeModel
	process *recoveryApprovalProcess
}

func (s *recoveryApprovalStore) hold() {
	m, err := state.NewManager(s.Store, "approval-crash")
	if err != nil {
		panic(err)
	}
	s.report.Durable = m.View()
	if s.report.TraceID == "" {
		for id := range s.report.Durable.Traces {
			s.report.TraceID = id
		}
	}
	s.report.ToolCalls, err = readEffect(s.process.effect)
	if err != nil {
		panic(err)
	}
	s.report.ReturnedCalls, err = readEffect(s.process.effect + "-returned")
	if err != nil {
		panic(err)
	}
	s.report.ModelCalls = s.model.Calls()
	raw, err := json.Marshal(s.report)
	if err != nil {
		panic(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, recoveryApprovalReady+string(raw)); err != nil {
		panic(err)
	}
	// No timeout or cooperative shutdown may advance the selected window.
	select {}
}

func (s *recoveryApprovalStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	kind := recoveryApprovalCommitKind(c)
	matches := s.window == kind+"_before" || s.window == kind+"_after"
	if matches {
		s.report.Target = c
		prefix, err := loadPrefix(s.Store, id)
		if err != nil {
			return store.CommitReceipt{}, err
		}
		s.report.Prefix = prefix
		if s.window == kind+"_before" {
			s.hold()
		}
	}
	receipt, err := s.Store.Append(ctx, id, expected, c)
	// An after barrier is after durable Append, before in-memory publication.
	if err == nil && matches && s.window == kind+"_after" {
		s.hold()
	}
	return receipt, err
}

func recoveryApprovalChild(t *testing.T, root, window string) {
	opts, model, process := recoveryApprovalOptions(t, root)
	created, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	header, err := readHeader(opts.StateRoot, opts.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := decodeBinding(header.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	opts.Workspace = binding.HostRealRoot
	backend, err := jsonl.Open(opts.SessionID, opts.StateRoot, header, jsonl.Options{OpenExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &recoveryApprovalStore{Store: backend, CheckpointBlobs: backend, window: window, model: model, process: process}
	process.barrier = wrapped
	manager, err := state.NewManager(wrapped, opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	opts.Store = wrapped
	s, err := Start(opts, manager, binding.Generation)
	if err != nil {
		t.Fatal(err)
	}
	receipt := submitOutput(t, s)
	waitResumeCondition(t, func() bool {
		tr := manager.View().Traces[receipt.TraceID]
		return tr.State == "paused" || terminal(tr.State)
	})
	snap := approvalSnapshot(t, s)
	if snap.Traces[receipt.TraceID].State != "paused" || len(snap.Interactions) != 1 || len(snap.Approvals) != 1 {
		t.Fatalf("fixture state=%s error=%s interactions=%d approvals=%d", snap.Traces[receipt.TraceID].State, snap.Traces[receipt.TraceID].Error, len(snap.Interactions), len(snap.Approvals))
	}
	var response InteractionResponse
	for id := range snap.Interactions {
		response = InteractionResponse{InteractionID: id, Decision: "allowed-once", ExpectedRevision: snap.Revision, IdempotencyKey: "answer"}
	}
	beforeAnswer := manager.View()
	if window != "asked" {
		response = answerCommand(t, s, "allowed-once")
	}
	if !reflect.DeepEqual(beforeAnswer, manager.View()) {
		t.Fatal("runtime approval answer changed durable facts")
	}
	if err := s.rt.do(t.Context(), func(rt *runtime) error {
		pending := rt.approvals[response.InteractionID]
		if pending == nil || (pending.decision == "allowed-once") != (window != "asked") {
			return fmt.Errorf("wrong runtime approval window")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wrapped.report = recoveryApprovalReport{Before: manager.View(), Response: response, TraceID: receipt.TraceID}
	if window == "asked" || window == "decided" {
		wrapped.hold()
	}
	if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: receipt.TraceID, ExpectedRevision: wrapped.report.Before.LastSeq, IdempotencyKey: "crash-resume"}); err != nil {
		t.Fatal(err)
	}
	// Later barriers run on the worker or mailbox. Keep the test alive until
	// the parent kills it, rather than interpreting accepted as completion.
	<-t.Context().Done()
	t.Fatal("requested approval crash window was not reached")
}

// These are real Kill/Wait windows, not Close/reopen or serialization tests.
// asked/decided are stopped checkpoints with process-local approval state;
// resume_after is durable acceptance BEFORE the resumed worker is started.
func TestRecoveryApprovalCrash(t *testing.T) {
	if window := os.Getenv("SEASPRAK_APPROVAL_CRASH_WINDOW"); window != "" {
		recoveryApprovalChild(t, os.Getenv("SEASPRAK_APPROVAL_CRASH_ROOT"), window)
		return
	}
	for _, window := range []string{"asked", "decided", "resume_before", "resume_after", "checkpoint_before", "checkpoint_after", "claim_before", "claim_after", "execute_entered", "execute_returned", "observation_before", "observation_after", "segment_before", "segment_after", "stopped_before", "stopped_after", "settled_before", "settled_after"} {
		t.Run(window, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "workspace"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "state"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "effect-returned"), []byte("0"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "effect"), []byte("0"), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryApprovalCrash$", "-test.count=1", "-test.timeout=60s")
			cmd.Env = append(os.Environ(), "SEASPRAK_APPROVAL_CRASH_WINDOW="+window, "SEASPRAK_APPROVAL_CRASH_ROOT="+root)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr, output bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			ready := make(chan recoveryApprovalReport, 1)
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				scanner := bufio.NewScanner(stdout)
				scanner.Buffer(make([]byte, 65536), 4<<20)
				for scanner.Scan() {
					line := scanner.Text()
					output.WriteString(line + "\n")
					if !strings.HasPrefix(line, recoveryApprovalReady) {
						continue
					}
					var rep recoveryApprovalReport
					if json.Unmarshal([]byte(strings.TrimPrefix(line, recoveryApprovalReady)), &rep) == nil {
						ready <- rep
					}
				}
			}()
			stopped := false
			stop := func() {
				if stopped {
					return
				}
				stopped = true
				_ = cmd.Process.Kill()
				select {
				case <-drained:
				case <-time.After(10 * time.Second):
					_ = stdout.Close()
					<-drained
				}
				_ = cmd.Wait()
			}
			t.Cleanup(stop)
			var rep recoveryApprovalReport
			select {
			case rep = <-ready:
			case <-drained:
				stop()
				t.Fatalf("child ended before barrier: %s\n%s", stderr.String(), output.String())
			case <-time.After(40 * time.Second):
				stop()
				t.Fatalf("barrier timeout: %s", stderr.String())
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			stop()
			if cmd.ProcessState.Success() {
				t.Fatal("child exited successfully instead of being killed")
			}
			if window != "asked" && window != "decided" && !strings.HasPrefix(window, "resume_") {
				assertRecoveryApprovalExtended(t, root, window, rep)
				return
			}
			if rep.ModelCalls != 1 {
				t.Fatalf("child model calls=%d", rep.ModelCalls)
			}
			opts, model, process := recoveryApprovalOptions(t, root)
			journal := journalPath(opts.StateRoot, opts.SessionID)
			beforeBytes, err := os.ReadFile(journal)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := loadCrashJournal(opts.StateRoot, opts.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(window, "resume_") {
				_, present := commitByID(stored, rep.Target.CommitID)
				if present != (window == "resume_after") {
					t.Fatal("resume commit presence disagrees with barrier")
				}
			}
			opened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = opened.Close(context.Background()) })
			// Writable Open persists running -> paused recovery metadata; this is
			// not an automatic Resume and must not authorize any execution.
			if window == "resume_after" {
				recovered, loadErr := loadCrashJournal(opts.StateRoot, opts.SessionID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if len(recovered.Commits) != len(stored.Commits)+1 || opened.rt.manager.View().Traces[rep.TraceID].State != "paused" {
					t.Fatal("accepted resume did not recover as one paused transition")
				}
				beforeBytes, err = os.ReadFile(journal)
				if err != nil {
					t.Fatal(err)
				}
			}
			snap := approvalSnapshot(t, opened)
			if len(snap.Interactions) != 0 || len(snap.Approvals) != 0 {
				t.Fatal("Open restored runtime permission or asked automatically")
			}
			v := opened.rt.manager.View()
			if !reflect.DeepEqual(rep.Before.Calls, v.Calls) || !reflect.DeepEqual(rep.Before.Observations, v.Observations) || !reflect.DeepEqual(rep.Before.Traces[rep.TraceID].Usage, v.Traces[rep.TraceID].Usage) {
				t.Fatal("Open changed original calls, observations or budget")
			}
			if len(v.Calls) != 1 {
				t.Fatalf("original calls=%d", len(v.Calls))
			}
			for _, call := range v.Calls {
				if call.Claimed || call.Observation != nil {
					t.Fatal("unstarted call acquired claim or observation")
				}
			}
			if v.Traces[rep.TraceID].Usage.ToolExecutions != 0 {
				t.Fatal("unstarted approval consumed tool budget")
			}
			if window == "resume_after" {
				if len(v.Operations) != len(rep.Before.Operations)+1 || len(v.ResumedExecutions) != len(rep.Before.ResumedExecutions)+1 {
					t.Fatal("durable resume acceptance lost")
				}
			} else if !reflect.DeepEqual(rep.Before.Operations, v.Operations) {
				t.Fatal("uncommitted resume acquired an operation")
			}
			rep.Response.ExpectedRevision = v.LastSeq
			_, err = opened.RespondInteraction(t.Context(), rep.Response)
			requireSessionCode(t, err, product.CodeNotFound)
			if !reflect.DeepEqual(v, opened.rt.manager.View()) {
				t.Fatal("stale approval response changed state")
			}
			if model.Calls() != 0 || process.calls.Load() != 0 {
				t.Fatal("Open or stale response started model/tool")
			}
			effects, err := readEffect(filepath.Join(root, "effect"))
			if err != nil || effects != 0 {
				t.Fatalf("durable tool calls=%d err=%v", effects, err)
			}
			if err := opened.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			afterBytes, err := os.ReadFile(journal)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeBytes, afterBytes) {
				t.Fatal("Open/Snapshot/stale response/Close wrote the journal")
			}
		})
	}
}
