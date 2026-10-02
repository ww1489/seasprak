package codeagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

// These windows are after the original execution observation. A saved log is
// not a published reference until its projection transaction commits.
func TestProjectionCrash(t *testing.T) {
	if os.Getenv("SEASPRAK_PROJECTION_CHILD") == "1" {
		runProjectionCrashChild(t)
		return
	}
	for _, window := range []string{"save_before", "save_after", "projection_before", "projection_after"} {
		t.Run(window, func(t *testing.T) { runProjectionCrashParent(t, window) })
	}
}

type projectionCrashArtifacts struct {
	*fixture.Memory
	root, window string
	model        *testkit.FakeModel
}

func (p *projectionCrashArtifacts) SaveOutput(ctx context.Context, in agent.OutputArtifactInput) (agent.ArtifactRef, error) {
	if p.window == "save_before" {
		projectionCrashHold(p.model)
	}
	if err := bumpEffect(filepath.Join(p.root, "saves")); err != nil {
		return agent.ArtifactRef{}, err
	}
	ref, err := p.Memory.SaveOutput(ctx, in)
	if err != nil {
		return ref, err
	}
	// A durable synthetic log payload makes the saved-but-unpublished window
	// observable after process death, independently of the memory fixture.
	raw, err := json.Marshal(in)
	if err != nil {
		return agent.ArtifactRef{}, err
	}
	f, err := os.OpenFile(filepath.Join(p.root, "log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return agent.ArtifactRef{}, err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return agent.ArtifactRef{}, err
	}
	if p.window == "save_after" {
		projectionCrashHold(p.model)
	}
	return ref, nil
}

type projectionCrashProcess struct{ effect string }

func (projectionCrashProcess) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return sessionFakeCapabilities("projection-crash-process"), nil
}

func (p projectionCrashProcess) Execute(ctx context.Context, in agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := in.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	if err := bumpEffect(p.effect); err != nil {
		return agent.ProcessObservation{}, err
	}
	return agent.ProcessObservation{Started: true, Terminated: true, SideEffect: "confirmed", Content: strings.Repeat("synthetic log line\n", 2100)}, nil
}
func (projectionCrashProcess) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}

type projectionCrashStore struct {
	store.Store
	window string
	model  *testkit.FakeModel
}

func (s *projectionCrashStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	target := false
	for _, r := range c.ControlRecords {
		if r.Type == "tool_output_projection" {
			target = true
		}
	}
	if target && s.window == "projection_before" {
		projectionCrashHold(s.model)
	}
	receipt, err := s.Store.Append(ctx, id, expected, c)
	if err == nil && target && s.window == "projection_after" {
		projectionCrashHold(s.model)
	}
	return receipt, err
}
func projectionCrashHold(model *testkit.FakeModel) {
	fmt.Printf("PROJECTION_READY %d\n", model.Calls())
	_ = os.Stdout.Sync()
	select {}
}
func projectionCrashOptions(t *testing.T, root, window string) (Options, *testkit.FakeModel) {
	t.Helper()
	model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider-log", Name: "execute", Arguments: `{"argv":["fixture-command"]}`}}}, testkit.Step{Text: "done"})
	artifacts := &projectionCrashArtifacts{Memory: fixture.NewMemory(), root: root, window: window, model: model}
	opts := Options{SessionID: "projection-crash", Workspace: filepath.Join(root, "workspace"), StateRoot: filepath.Join(root, "state"), Profile: ProfileMemory, GenerationFingerprint: "projection-crash-v1", Model: versionedPauseModel{model}, Tools: []tools.Definition{builtinDefinitionForSession(t, "execute")}, Operations: tools.Operations{Process: projectionCrashProcess{filepath.Join(root, "effects")}, Artifacts: artifacts, OutputRedactor: func(_ context.Context, text string) (string, error) { return text, nil }}, ResourceScheduler: tools.NewResourceScheduler()}
	return opts, model
}
func runProjectionCrashChild(t *testing.T) {
	root, window := os.Getenv("SEASPRAK_PROJECTION_ROOT"), os.Getenv("SEASPRAK_PROJECTION_WINDOW")
	opts, model := projectionCrashOptions(t, root, window)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	initial, err := CreateAgentSession(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = initial.Close(ctx); err != nil {
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
	if _, err = alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	opts.Workspace = binding.HostRealRoot
	backend, err := jsonl.Open(opts.SessionID, opts.StateRoot, header, jsonl.Options{OpenExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &projectionCrashStore{Store: backend, window: window, model: model}
	manager, err := state.NewManager(wrapped, opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	opts.Store = wrapped
	session, err := Start(opts, manager, binding.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.SubmitInput(ctx, agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run fixture"}`)}); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	t.Fatal("projection crash barrier not reached")
}
func runProjectionCrashParent(t *testing.T, window string) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"effects", "saves"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("0"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProjectionCrash$", "-test.count=1", "-test.timeout=45s")
	cmd.Env = append(os.Environ(), "SEASPRAK_PROJECTION_CHILD=1", "SEASPRAK_PROJECTION_ROOT="+root, "SEASPRAK_PROJECTION_WINDOW="+window)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	ready := make(chan int, 1)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			output.WriteString(scanner.Text() + "\n")
			var n int
			if _, err := fmt.Sscanf(scanner.Text(), "PROJECTION_READY %d", &n); err == nil {
				select {
				case ready <- n:
				default:
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
		_ = cmd.Process.Kill()
		<-drained
		_ = cmd.Wait()
	}
	t.Cleanup(stop)
	select {
	case calls := <-ready:
		if calls != 1 {
			t.Errorf("model calls=%d want1", calls)
		}
	case <-drained:
		stop()
		t.Fatalf("child exited before barrier: %s %s", stderr.String(), output.String())
	case <-time.After(35 * time.Second):
		stop()
		t.Fatal("crash barrier timeout")
	}
	stop()
	if cmd.ProcessState.Success() {
		t.Fatal("child was not killed")
	}
	opts, model := projectionCrashOptions(t, root, "")
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	view := opened.rt.manager.View()
	if model.Calls() != 0 || mustReadEffect(t, filepath.Join(root, "effects")) != 1 {
		t.Fatal("Open repeated execution")
	}
	wantSaves := 1
	if window == "save_before" {
		wantSaves = 0
	}
	if mustReadEffect(t, filepath.Join(root, "saves")) != wantSaves {
		t.Fatal("unexpected artifact save count")
	}
	_, logErr := os.Stat(filepath.Join(root, "log"))
	if (logErr == nil) != (wantSaves == 1) {
		t.Fatal("saved payload does not match save window")
	}
	if len(view.Calls) != 1 {
		t.Fatal("missing original call")
	}
	for _, call := range view.Calls {
		if !call.Claimed || call.Observation == nil || call.Observation.Status != "succeeded" || call.Observation.SideEffect != "confirmed" || !call.Observation.Terminated {
			t.Fatal("durable original observation lost")
		}
		tr := view.Traces[call.Scope.TraceID]
		if tr.State != "paused" || tr.Usage.ToolExecutions != 1 || tr.Usage.LogicalModelCalls != 1 {
			t.Fatal("reopen changed trace or budget")
		}
		p, exists := view.ToolProjections[call.Call.CallID]
		if exists != (window == "projection_after") {
			t.Fatal("uncommitted reference published or committed projection lost")
		}
		if exists && (p.Artifact.ID == "" || !p.Truncated || p.Observation != *call.Observation) {
			t.Fatal("projection changed original observation or lost artifact")
		}
	}
	if err = opened.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if model.Calls() != 0 || mustReadEffect(t, filepath.Join(root, "effects")) != 1 || mustReadEffect(t, filepath.Join(root, "saves")) != wantSaves {
		t.Fatal("browse/close caused replay")
	}
}
