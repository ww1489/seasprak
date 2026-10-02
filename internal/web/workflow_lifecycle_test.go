package web

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
	"github.com/ww1489/seasprak/internal/workflowagent"
)

type countedCloseRegistry struct {
	storage.CreationRegistry
	closed atomic.Int32
}

func (r *countedCloseRegistry) Close() error { r.closed.Add(1); return r.CreationRegistry.Close() }
func TestResourceCatalogsSharedRegistryWaitsForBothWritersAndAdmittedCodeIO(t *testing.T) {
	workflowEntered, workflowCancelled, workflowRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	codeRelease := make(chan struct{})
	defer func() {
		for _, gate := range []chan struct{}{workflowRelease, codeRelease} {
			select {
			case <-gate:
			default:
				close(gate)
			}
		}
	}()
	var workflowCalls atomic.Int32
	tool := tools.Definition{Name: "probe", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "none"}, Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		workflowCalls.Add(1)
		close(workflowEntered)
		<-ctx.Done()
		close(workflowCancelled)
		<-workflowRelease
		return "", ctx.Err()
	}}
	runtime, conf := workflowRuntimeFixture(t, workflowToolDefinition("probe"), tool)
	codeModel := &catalogUncooperativeModel{FakeModel: testkit.NewFake(testkit.Step{Gate: codeRelease, Text: "done"}), started: make(chan struct{}), cancelled: make(chan struct{})}
	runtime.Code.Model = codeModel
	catalogs, server, token := runtimeHTTP(t, runtime)
	registry := &countedCloseRegistry{CreationRegistry: catalogs.registry}
	installWorkflowRegistry(catalogs, registry)
	code, err := catalogs.code.Create(t.Context(), CatalogCreateRequest{Workspace: conf.Workspace, ModelRef: DefaultModelRef, IdempotencyKey: "code"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := catalogs.code.Writer(t.Context(), code.Snapshot.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"wait"}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-codeModel.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Code model did not enter")
	}
	status, created := call(t, server, token, "POST", "/v1/workflow-runs", "workflow", toolCreateBody(conf))
	if status != 202 {
		t.Fatalf("workflow status=%d", status)
	}
	rid := created["runId"].(string)
	select {
	case <-workflowEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("Workflow tool did not enter")
	}
	fileGate := newCatalogPublicationGate(t)
	defer fileGate.unblock()
	catalogs.code.publish = fileGate.publish
	published := make(chan error, 1)
	go func() {
		published <- fileMutation(catalogs.code, context.Background(), code.Snapshot.SessionID, "metadata", "pending")
	}()
	waitPublication(t, fileGate)
	closeShort := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		err := catalogs.Close(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("close deadline=%v", err)
		}
		if registry.closed.Load() != 0 {
			t.Fatal("shared registry closed before all admitted work exited")
		}
		if other, err := storage.OpenCreationRegistry(conf.StateRoot); err == nil {
			_ = other.Close()
			t.Fatal("shared registry lock released early")
		}
	}
	closeShort()
	select {
	case <-codeModel.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Code Close did not reach actual runner")
	}
	select {
	case <-workflowCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Workflow Close did not reach actual runner")
	}
	close(codeRelease)
	// The Code catalog may release its session writer, but not the shared owner.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err = catalogs.code.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending Code file IO Close=%v", err)
	}
	catalogs.code.mu.Lock()
	writers, active := len(catalogs.code.writers), catalogs.code.activeFileWrites
	catalogs.code.mu.Unlock()
	if writers != 0 || active != 1 {
		t.Fatalf("Code writer/file counts=%d/%d", writers, active)
	}
	closeShort()
	fileGate.unblock()
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	if err := catalogs.code.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if registry.closed.Load() != 0 {
		t.Fatal("borrowed Code catalog closed shared registry")
	}
	closeShort()
	close(workflowRelease)
	if err := catalogs.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if registry.closed.Load() != 1 || codeModel.Calls() != 1 || workflowCalls.Load() != 1 {
		t.Fatalf("registry/Code/Workflow counts=%d/%d/%d", registry.closed.Load(), codeModel.Calls(), workflowCalls.Load())
	}
	other, err := storage.OpenCreationRegistry(conf.StateRoot)
	if err != nil {
		t.Fatal("registry lock not released after actual exits")
	}
	_ = other.Close()
	opts := runtime.Workflows["tool@v1"]
	opts.RunID = rid
	reopened, err := workflowagent.OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal("Workflow writer retained after real exit")
	}
	_ = reopened.Close(t.Context())
}

type countedWorkflowCloseErrorStore struct {
	catalogCloseErrorStore
	closed atomic.Int32
}

func (s *countedWorkflowCloseErrorStore) Close() error {
	s.closed.Add(1)
	return s.catalogCloseErrorStore.Close()
}

func TestResourceCatalogsCloseJoinsCodeWorkflowAndRegistryErrors(t *testing.T) {
	runtime, conf, _ := modelRuntime(t)
	catalogs, server, token := runtimeHTTP(t, runtime)
	status, created := call(t, server, token, "POST", "/v1/workflow-runs", "create", workflowCreateBody(conf))
	if status != 202 {
		t.Fatalf("workflow status=%d", status)
	}
	rid := created["runId"].(string)
	pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["executionStopped"] == true })
	old := catalogs.workflows.writers[rid]
	if err := old.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	backend, err := jsonl.Open(rid, conf.StateRoot, storage.Header{}, jsonl.Options{OpenExisting: true, ResourceType: storage.ResourceWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	codeErr, workflowErr, registryErr := errors.New("fixture Code close failed"), errors.New("fixture Workflow close failed"), errors.New("fixture registry close failed")
	opts := runtime.Workflows["constant@v1"]
	opts.RunID = rid
	backendOwner := &countedWorkflowCloseErrorStore{catalogCloseErrorStore: catalogCloseErrorStore{Store: backend, err: workflowErr}}
	opts.Store = backendOwner
	run, err := workflowagent.OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		_ = backend.Close()
		t.Fatal(err)
	}
	catalogs.workflows.writers[rid] = run
	exitedCatalogErrorSession(t, catalogs.code, codeErr)
	registry := &closeErrorRegistry{CreationRegistry: catalogs.registry, err: registryErr}
	installWorkflowRegistry(catalogs, registry)
	err = catalogs.Close(t.Context())
	workflowClose := run.Close(t.Context())
	pe, ok := product.AsError(workflowClose)
	if !ok || pe.Code != product.CodeStorageUnavailable || pe.Message != "workflow close failed" || pe.Details != nil || len(pe.Refs) != 0 {
		t.Fatal("Workflow Close did not preserve its direct safe product error")
	}
	for _, want := range []error{codeErr, workflowClose, registryErr} {
		if !errors.Is(err, want) {
			t.Fatalf("combined owner lost cleanup error %v: %v", want, err)
		}
	}
	if errors.Is(err, workflowErr) || strings.Contains(err.Error(), workflowErr.Error()) {
		t.Fatal("combined owner exposed the private Workflow backend error")
	}
	if again := run.Close(t.Context()); again != workflowClose || backendOwner.closed.Load() != 1 {
		t.Fatal("repeated Workflow Close changed its safe result or closed the backend again")
	}
	if len(catalogs.code.writers) != 0 || len(catalogs.workflows.writers) != 0 {
		t.Fatal("actual-exited writer retained after cleanup error")
	}
	other, err := storage.OpenCreationRegistry(conf.StateRoot)
	if err != nil {
		t.Fatal("registry retained after real exits with cleanup errors")
	}
	_ = other.Close()
	opts.Store = nil
	reopened, err := workflowagent.OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal("Workflow writer lock retained after the actual backend Close")
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	if err := reopened.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Logf("safeWorkflowErrorAggregated=true privateBackendErrorExposed=false backendClose=%d writers=0 registryAndWorkflowLocksReleased=true", backendOwner.closed.Load())
}
