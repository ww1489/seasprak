package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
	"github.com/ww1489/seasprak/internal/workflowagent"
)

func httpModelWorkflow() workflowagent.WorkflowDefinition {
	def := httpLiteralWorkflow()
	def.Nodes[1] = workflowagent.WorkflowNode{ID: "model", Type: "model", Model: "default", Prompt: "private-model-prompt-marker"}
	def.Nodes[2].Inputs = map[string]workflowagent.WorkflowValue{"result": {Ref: &workflowagent.WorkflowRef{Node: "model", Field: "text"}}}
	def.Edges = []workflowagent.WorkflowEdge{{From: "start", To: "model"}, {From: "model", To: "end"}}
	return def
}
func modelRuntime(t *testing.T) (runtimeOptions, Config, *testkit.FakeModel) {
	t.Helper()
	code, _ := catalogOptions(t)
	fake := testkit.NewFake(testkit.Step{Text: "chosen model result"})
	def := httpModelWorkflow()
	opts := workflowagent.WorkflowOptions{Workspace: code.Workspace, StateRoot: code.StateRoot, Principal: localPrincipal, Definition: def, GenerationFingerprint: "independent-model-binding-v1", Models: map[string]model.AgenticModel{"default": fake}}
	return runtimeOptions{Code: code, Workflows: map[string]workflowagent.WorkflowOptions{def.Name + "@" + def.Version: opts}}, Config{Workspace: code.Workspace, StateRoot: code.StateRoot}, fake
}

type workflowFaultRegistry struct {
	storage.CreationRegistry
	failBefore, failAfter bool
	onReceipt             func(context.Context)
	calls                 atomic.Int32
}

func (r *workflowFaultRegistry) Save(ctx context.Context, key string, record storage.CreationRecord) error {
	r.calls.Add(1)
	if len(record.Receipt) > 0 {
		if r.onReceipt != nil {
			r.onReceipt(ctx)
		}
		if r.failBefore {
			r.failBefore = false
			return product.NewError(product.CodeStorageUnavailable, "fixture receipt publication failed")
		}
		if r.failAfter {
			r.failAfter = false
			err := r.CreationRegistry.Save(ctx, key, record)
			if err != nil {
				return err
			}
			return product.NewError(product.CodeStorageUnavailable, "fixture receipt publication outcome unknown")
		}
	}
	return r.CreationRegistry.Save(ctx, key, record)
}
func installWorkflowRegistry(c *resourceCatalogs, r storage.CreationRegistry) {
	c.registry = r
	c.code.registry = r
	c.workflows.registry = r
}
func TestWorkflowCatalogLostReceiptReopenPreservesAcceptanceAndOneModelCall(t *testing.T) {
	for _, mode := range []string{"before_publication", "after_publication"} {
		t.Run(mode, func(t *testing.T) {
			runtime, conf, fake := modelRuntime(t)
			catalogs, server, token := runtimeHTTP(t, runtime)
			fault := &workflowFaultRegistry{CreationRegistry: catalogs.registry, failBefore: mode == "before_publication", failAfter: mode == "after_publication"}
			installWorkflowRegistry(catalogs, fault)
			status, out := call(t, server, token, "POST", "/v1/workflow-runs", "lost", workflowCreateBody(conf))
			assertWorkflowError(t, status, out, 503, "storage_unavailable")
			record, found, err := catalogs.registry.Load(t.Context(), workflowRegistryKey(localPrincipal, "lost"))
			if err != nil || !found || record.ResourceType != storage.ResourceWorkflow || record.RunID == "" || len(catalogs.workflows.writers) != 1 {
				t.Fatal("lost receipt lost reservation or writer")
			}
			final := pollWorkflowHTTP(t, server, token, record.RunID, func(v map[string]any) bool { return v["state"] == "completed" && v["executionStopped"] == true })
			if fake.Calls() != 1 {
				t.Fatalf("actual model count=%d", fake.Calls())
			}
			if err := catalogs.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			catalogs, server, token = runtimeHTTP(t, runtime)
			journal := filepath.Join(conf.StateRoot, "workflow-runs", record.RunID, "journal.jsonl")
			before, err := os.ReadFile(journal)
			if err != nil {
				t.Fatal(err)
			}
			status, initial := call(t, server, token, "POST", "/v1/workflow-runs", "lost", workflowCreateBody(conf))
			if status != 202 || initial["runId"] != record.RunID || initial["state"] != "running" || initial["revision"].(float64) >= final["revision"].(float64) || initial["durableSeq"] != "3" || len(initial["workflowNodes"].([]any)) != 0 {
				t.Fatalf("retry invented later receipt: %d %v", status, initial)
			}
			status, again := call(t, server, token, "POST", "/v1/workflow-runs", "lost", workflowCreateBody(conf))
			if status != 202 || !reflect.DeepEqual(initial, again) || fake.Calls() != 1 {
				t.Fatal("receipt retry repeated model or changed response")
			}
			after, err := os.ReadFile(journal)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("receipt recovery executed or changed journal")
			}
			entries, err := os.ReadDir(filepath.Join(conf.StateRoot, "workflow-runs"))
			if err != nil || len(entries) != 1 {
				t.Fatal("receipt recovery allocated another run")
			}
		})
	}
}
func TestWorkflowCatalogReservationSurvivesBeforeMaterializationAndTypedIDs(t *testing.T) {
	for _, emptyDir := range []bool{false, true} {
		t.Run(map[bool]string{false: "reservation", true: "empty_resource_directory"}[emptyDir], func(t *testing.T) {
			runtime, conf, fake := modelRuntime(t)
			catalogs, server, token := runtimeHTTP(t, runtime)
			rid := "same-resource-id"
			req := workflowCreateRequest{Workspace: conf.Workspace, Workflow: "constant", Version: "v1", Input: json.RawMessage(`{"name":"one"}`), IdempotencyKey: "reserved"}
			record := storage.CreationRecord{ResourceType: storage.ResourceWorkflow, RunID: rid, Digest: catalogs.workflows.digest(req), Generation: runtime.Workflows["constant@v1"].GenerationFingerprint}
			if err := catalogs.registry.Save(t.Context(), workflowRegistryKey(localPrincipal, "reserved"), record); err != nil {
				t.Fatal(err)
			}
			codeRequest := CatalogCreateRequest{Workspace: conf.Workspace, ModelRef: DefaultModelRef, IdempotencyKey: "reserved"}
			if err := catalogs.registry.Save(t.Context(), registryKey(localPrincipal, "reserved"), storage.CreationRecord{SessionID: rid, Digest: catalogs.code.digest(codeRequest), Generation: runtime.Code.GenerationFingerprint}); err != nil {
				t.Fatal(err)
			}
			if emptyDir {
				if _, err := storage.PrepareResourceDir(conf.StateRoot, storage.ResourceWorkflow, rid); err != nil {
					t.Fatal(err)
				}
			}
			if err := catalogs.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			catalogs, server, token = runtimeHTTP(t, runtime)
			status, workflow := call(t, server, token, "POST", "/v1/workflow-runs", "reserved", workflowCreateBody(conf))
			if status != 202 || workflow["runId"] != rid {
				t.Fatalf("reserved run status=%d %v", status, workflow)
			}
			pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["executionStopped"] == true })
			status, code := call(t, server, token, "POST", "/v1/sessions", "reserved", map[string]any{"workspace": conf.Workspace})
			if status != 200 || code["sessionId"] != rid {
				t.Fatalf("same id Code reservation status=%d %v", status, code)
			}
			if fake.Calls() != 1 {
				t.Fatal("reserved run model count mismatch")
			}
			if len(catalogs.workflows.writers) != 1 || len(catalogs.code.writers) != 1 {
				t.Fatal("typed namespaces share writer map")
			}
		})
	}
}
func TestWorkflowCatalogReservationRejectsForeignBinding(t *testing.T) {
	runtime, conf, _ := modelRuntime(t)
	foreign := runtime.Workflows["constant@v1"]
	foreign.RunID = "foreign-run"
	foreign.Workspace = t.TempDir()
	run, err := workflowagent.CreateWorkflowAgent(t.Context(), foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	catalogs, server, token := runtimeHTTP(t, runtime)
	request := workflowCreateRequest{Workspace: conf.Workspace, Workflow: "constant", Version: "v1", Input: json.RawMessage(`{"name":"one"}`), IdempotencyKey: "foreign"}
	record := storage.CreationRecord{ResourceType: storage.ResourceWorkflow, RunID: foreign.RunID, Digest: catalogs.workflows.digest(request), Generation: foreign.GenerationFingerprint}
	if err := catalogs.registry.Save(t.Context(), workflowRegistryKey(localPrincipal, "foreign"), record); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(conf.StateRoot, "workflow-runs", foreign.RunID, "journal.jsonl")
	before, _ := os.ReadFile(path)
	status, out := call(t, server, token, "POST", "/v1/workflow-runs", "foreign", workflowCreateBody(conf))
	assertWorkflowError(t, status, out, 409, "state_conflict")
	status, _ = call(t, server, token, "GET", "/v1/workflow-runs/"+foreign.RunID, "", nil)
	if status != 404 {
		t.Fatalf("foreign browse status=%d", status)
	}
	status, out = call(t, server, token, "GET", "/v1/workflow-runs", "", nil)
	if status != 200 || len(out["runs"].([]any)) != 0 {
		t.Fatal("foreign workspace appeared in list")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || len(catalogs.workflows.writers) != 0 {
		t.Fatal("foreign reservation acquired writer or changed journal")
	}
}
func TestWorkflowCatalogOldGenerationRefusesWritesButAllowsMissingDefinitionBrowse(t *testing.T) {
	runtime, conf, fake := modelRuntime(t)
	catalogs, server, token := runtimeHTTP(t, runtime)
	_, initial := call(t, server, token, "POST", "/v1/workflow-runs", "create", workflowCreateBody(conf))
	rid := initial["runId"].(string)
	pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["executionStopped"] == true })
	if err := catalogs.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opts := runtime.Workflows["constant@v1"]
	opts.GenerationFingerprint = "new-generation"
	runtime.Workflows["constant@v1"] = opts
	catalogs, server, token = runtimeHTTP(t, runtime)
	status, out := call(t, server, token, "POST", "/v1/workflow-runs", "create", workflowCreateBody(conf))
	assertWorkflowError(t, status, out, 409, "idempotency_conflict")
	status, out = call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/open", "", map[string]any{})
	assertWorkflowError(t, status, out, 409, "incompatible_version")
	if err := catalogs.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	runtime.Workflows = nil
	catalogs, server, token = runtimeHTTP(t, runtime)
	status, out = call(t, server, token, "GET", "/v1/workflow-runs/"+rid+"/snapshot", "", nil)
	if status != 200 || out["runId"] != rid || len(catalogs.workflows.writers) != 0 || fake.Calls() != 1 {
		t.Fatal("missing definition prevented zero-execution browse")
	}
	status, out = call(t, server, token, "POST", "/v1/workflow-runs/"+rid+"/open", "", map[string]any{})
	assertWorkflowError(t, status, out, 409, "incompatible_version")
}
func TestWorkflowHTTPDisconnectAfterAcceptanceDoesNotCancel(t *testing.T) {
	started, release, saveEntered, saveRelease := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		select {
		case <-saveRelease:
		default:
			close(saveRelease)
		}
	}()
	var effects atomic.Int32
	var cancelled atomic.Bool
	tool := tools.Definition{Name: "probe", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "none"}, Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		effects.Add(1)
		close(started)
		select {
		case <-ctx.Done():
			cancelled.Store(true)
			return "", ctx.Err()
		case <-release:
			return "done", nil
		}
	}}
	runtime, conf := workflowRuntimeFixture(t, workflowToolDefinition("probe"), tool)
	catalogs, server, token := runtimeHTTP(t, runtime)
	fault := &workflowFaultRegistry{CreationRegistry: catalogs.registry, onReceipt: func(ctx context.Context) {
		if err := ctx.Err(); err != nil {
			t.Error("accepted context cancelled")
		}
		close(saveEntered)
		<-saveRelease
		if err := ctx.Err(); err != nil {
			t.Error("publication used cancelled HTTP context")
		}
	}}
	installWorkflowRegistry(catalogs, fault)
	raw, _ := json.Marshal(toolCreateBody(conf))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "POST", server.URL()+"/v1/workflow-runs", bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", "disconnect")
	returned := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		returned <- err
	}()
	select {
	case <-saveEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("receipt save not reached")
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool not reached")
	}
	cancel()
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatalf("client error=%v", err)
	}
	close(saveRelease)
	// Wait for the original durable receipt without acquiring the creation lock.
	var record storage.CreationRecord
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		record, _, err = catalogs.registry.Load(t.Context(), workflowRegistryKey(localPrincipal, "disconnect"))
		if err != nil {
			t.Fatal(err)
		}
		if len(record.Receipt) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("accepted receipt not saved")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	final := pollWorkflowHTTP(t, server, token, record.RunID, func(v map[string]any) bool { return v["executionStopped"] == true })
	if final["state"] != "completed" || cancelled.Load() || effects.Load() != 1 {
		t.Fatal("HTTP disconnect cancelled accepted execution")
	}
	status, again := call(t, server, token, "POST", "/v1/workflow-runs", "disconnect", toolCreateBody(conf))
	if status != 202 || again["runId"] != record.RunID {
		t.Fatal("disconnect retry allocated another run")
	}
	if strings.Contains(string(record.Receipt), "private-argument-marker") {
		t.Fatal("receipt stored private arguments")
	}
}
