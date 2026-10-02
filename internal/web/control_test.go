package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	"github.com/ww1489/seasprak/internal/config"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

// reopenedControlCatalog uses a real JSONL journal and creation registry, but
// no network model or host tools. The creating writer is closed before HTTP.
func reopenedControlCatalog(t *testing.T, seed ...func(codeagent.Options, string)) (*Catalog, *Server, string, string, string, *testkit.FakeModel, *atomic.Int32) {
	t.Helper()
	model := testkit.NewFake()
	toolCalls := &atomic.Int32{}
	opts := codeagent.Options{
		Workspace: t.TempDir(), StateRoot: t.TempDir(), Principal: localPrincipal,
		Profile: codeagent.ProfileMemory, Model: model, GenerationFingerprint: "web-control-test-v1",
		Tools: []tools.Definition{{Name: "probe", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
			toolCalls.Add(1)
			return "done", nil
		}}},
	}
	catalog, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	created, err := catalog.Create(t.Context(), CatalogCreateRequest{Workspace: opts.Workspace, ModelRef: DefaultModelRef, IdempotencyKey: "create"})
	if err != nil {
		_ = catalog.Close(t.Context())
		t.Fatal(err)
	}
	sid := created.Snapshot.SessionID
	if err = catalog.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, write := range seed {
		write(opts, sid)
	}
	catalog, err = NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := catalog.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	token := rand.Text()
	httpServer := httptest.NewUnstartedServer(nil)
	httpServer.Config.Handler = authorize(httpServer.Listener.Addr().String(), token, newRoutes(catalog, localPrincipal))
	httpServer.Start()
	t.Cleanup(httpServer.Close)
	journal := filepath.Join(opts.StateRoot, "sessions", sid, "journal.jsonl")
	return catalog, &Server{url: httpServer.URL, options: opts}, token, sid, journal, model, toolCalls
}

func assertControlLive(t *testing.T, c *Catalog, sid string, want bool) {
	t.Helper()
	sub, live, release, err := c.Subscribe(t.Context(), sid, 0, config.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if sub == nil || live != want {
		t.Fatalf("subscription live=%t, want %t", live, want)
	}
}

func TestControlReadRoutesDoNotOpenWriter(t *testing.T) {
	catalog, server, token, sid, journal, model, calls := reopenedControlCatalog(t)
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/v1/sessions", "/v1/sessions/" + sid, "/v1/sessions/" + sid + "/snapshot",
		"/v1/sessions/" + sid + "/capabilities", "/v1/sessions/" + sid + "/render",
		"/v1/sessions/" + sid + "/workflows", "/v1/sessions/" + sid + "/branches",
		"/v1/sessions/" + sid + "/messages", "/v1/sessions/" + sid + "/metadata",
		"/v1/sessions/" + sid + "/traces/missing", "/v1/sessions/" + sid + "/attachments/missing",
		"/v1/sessions/" + sid + "/operations/missing",
	} {
		status, out := call(t, server, token, "GET", path, "", nil)
		want := 200
		if strings.HasSuffix(path, "/missing") || strings.HasSuffix(path, "/workflows") {
			want = 404
		}
		if status != want {
			t.Fatalf("GET %s status=%d %v", path, status, out)
		}
		assertControlLive(t, catalog, sid, false)
	}
	for _, suffix := range []string{"/events", "/ui/events"} {
		frames, status := readFrames(t, server, token, "/v1/sessions/"+sid+suffix, "", func(f []sseFrame) bool { return f[len(f)-1].event == "end" })
		if status != 200 || len(frames) < 2 || !strings.Contains(frames[0].data, `"live":false`) {
			t.Fatalf("read-only stream status=%d", status)
		}
	}
	after, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(before, after) || model.Calls() != 0 || calls.Load() != 0 {
		t.Fatal("read routes wrote history or invoked execution")
	}
}

func TestControlOpenReusesWriterWithoutExecutingOrChangingHistory(t *testing.T) {
	catalog, server, token, sid, journal, model, calls := reopenedControlCatalog(t)
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	assertControlLive(t, catalog, sid, false)
	status, opened := call(t, server, token, "POST", "/v1/sessions/"+sid+"/open", "", map[string]any{})
	if status != 200 || opened["sessionId"] != sid || opened["instanceId"] != catalog.InstanceID() || opened["cursor"] == "" {
		t.Fatalf("open status=%d snapshot=%v", status, opened)
	}
	writer, err := catalog.Writer(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			status, again := call(t, server, token, "POST", "/v1/sessions/"+sid+"/open", "", map[string]any{})
			if status != 200 || again["revision"] != opened["revision"] || again["instanceId"] != opened["instanceId"] {
				t.Errorf("repeated open status=%d", status)
			}
		})
	}
	wg.Wait()
	reader, release, err := catalog.Browse(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if reader != writer {
		t.Fatal("open created another writer")
	}
	assertControlLive(t, catalog, sid, true)
	for _, suffix := range []string{"/events", "/ui/events"} {
		frames, status := readFrames(t, server, token, "/v1/sessions/"+sid+suffix, "", func(f []sseFrame) bool { return f[0].event == "ready" })
		if status != 200 || !strings.Contains(frames[0].data, `"live":true`) {
			t.Fatalf("opened stream status=%d", status)
		}
	}
	_, authoritative := call(t, server, token, "GET", "/v1/sessions/"+sid+"/snapshot", "", nil)
	if authoritative["revision"] != opened["revision"] || authoritative["sessionId"] != sid {
		t.Fatal("open did not return the authoritative snapshot")
	}
	for _, private := range []string{"Principal", "Workspace", "Generation", "Model", "Tools", "Operations", "FrozenExecutions", "principal", "workspace", "generation", "model", "tools", "operations", "frozenExecutions", "stateRoot"} {
		if _, ok := opened[private]; ok {
			t.Fatalf("open leaked private field %s", private)
		}
	}
	after, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(before, after) || model.Calls() != 0 || calls.Load() != 0 {
		t.Fatal("opening control changed binding/history or invoked execution")
	}
}

func TestControlOpenPreservesPausedExecutionAndHeldQueue(t *testing.T) {
	var paused, held, oid string
	catalog, server, token, sid, journal, model, calls := reopenedControlCatalog(t, func(opts codeagent.Options, sid string) {
		backend, err := jsonl.Open(sid, opts.StateRoot, store.Header{}, jsonl.Options{OpenExisting: true})
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Close()
		manager, err := state.NewManager(backend, sid)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := backend.Load(t.Context(), sid)
		if err != nil {
			t.Fatal(err)
		}
		var binding struct {
			Generation string `json:"generation"`
		}
		if err := json.Unmarshal(stored.Header.Workspace, &binding); err != nil || binding.Generation == "" {
			t.Fatalf("created session generation is missing: %v", err)
		}
		target := agent.TargetAgent{Name: "main", Version: "v1", Generation: binding.Generation}
		for i := range 2 {
			receipt, err := manager.Accept(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"preserved input"}`), Principal: localPrincipal}, target)
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				paused = receipt.TraceID
			} else {
				held = receipt.TraceID
			}
		}
		if err := manager.HoldIndependent(t.Context(), held); err != nil {
			t.Fatal(err)
		}
		if err := manager.SetTraceState(t.Context(), paused, "running", false); err != nil {
			t.Fatal(err)
		}
		view := manager.View()
		op, err := manager.AcceptOperation(t.Context(), state.OperationCommand{Principal: localPrincipal, Kind: "pause", Target: paused, ExpectedRevision: view.LastSeq, IdempotencyKey: "pause"})
		if err != nil {
			t.Fatal(err)
		}
		oid = op.OperationID
		// This stored stopped proof is deliberately not a runnable checkpoint.
		// Opening control must retain it, not manufacture a recovery execution.
		if err := manager.CommitPause(t.Context(), paused, oid, state.CheckpointRef{ID: "stored-checkpoint", Scope: agent.ExecutionScope{SessionID: sid, TraceID: paused, InvocationID: view.Traces[paused].InvocationID, ExecutionID: "stored-execution"}}); err != nil {
			t.Fatal(err)
		}
	})
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	status, receipt := call(t, server, token, "GET", "/v1/sessions/"+sid+"/operations/"+oid, "", nil)
	if status != 200 || receipt["state"] != "completed" || receipt["result"] != "stored-checkpoint" {
		t.Fatalf("stored operation status=%d %v", status, receipt)
	}
	assertControlLive(t, catalog, sid, false)
	for range 2 {
		status, snapshot := call(t, server, token, "POST", "/v1/sessions/"+sid+"/open", "", map[string]any{})
		if status != 200 || traceState(snapshot, paused) != "paused" || traceState(snapshot, held) != "queued" {
			t.Fatalf("stored execution changed after open status=%d %v", status, snapshot)
		}
		current, err := catalog.Snapshot(t.Context(), sid)
		if err != nil || !current.Traces[held].Hold || !current.Traces[paused].ExecutionStopped || len(current.Approvals) != 0 {
			t.Fatalf("opening changed queue/stopped proof/approvals: %v", err)
		}
	}
	after, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(before, after) || model.Calls() != 0 || calls.Load() != 0 {
		t.Fatal("opening persisted work executed, approved, or changed journal history")
	}
}

func TestControlOpenOnlyNormalizesInterruptedRunningAndUnheldQueue(t *testing.T) {
	var running, queued string
	var original state.View
	catalog, server, token, sid, journal, model, calls := reopenedControlCatalog(t, func(opts codeagent.Options, sid string) {
		backend, err := jsonl.Open(sid, opts.StateRoot, store.Header{}, jsonl.Options{OpenExisting: true})
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Close()
		stored, err := backend.Load(t.Context(), sid)
		if err != nil {
			t.Fatal(err)
		}
		var binding struct {
			Generation string `json:"generation"`
		}
		if err := json.Unmarshal(stored.Header.Workspace, &binding); err != nil || binding.Generation == "" {
			t.Fatalf("created session generation is missing: %v", err)
		}
		manager, err := state.NewManager(backend, sid)
		if err != nil {
			t.Fatal(err)
		}
		target := agent.TargetAgent{Name: "main", Version: "v1", Generation: binding.Generation}
		for i := range 2 {
			receipt, err := manager.Accept(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"preserved input"}`), Principal: localPrincipal}, target)
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				running = receipt.TraceID
			} else {
				queued = receipt.TraceID
			}
		}
		if err := manager.SetTraceState(t.Context(), running, "running", false); err != nil {
			t.Fatal(err)
		}
		if err := manager.AppendMessage(t.Context(), userMessage("stored-user", running, "preserved parent message")); err != nil {
			t.Fatal(err)
		}
		invocation := manager.View().Traces[running].InvocationID
		turn := agent.TurnRecord{ID: "stored-turn", TraceID: running, InvocationID: invocation, ToolNames: []string{"probe"}}
		if err := manager.SaveTurn(t.Context(), turn); err != nil {
			t.Fatal(err)
		}
		message := assistantMessage("stored-assistant", running, "preserved assistant message")
		message.Scope.TurnID = turn.ID
		call := agent.ToolRecord{
			Call:  agent.FrozenCall{CallID: "stored-call", ProviderCallID: "stored-provider-call", Name: "probe", Arguments: "{}", Generation: binding.Generation},
			Scope: agent.ExecutionScope{SessionID: sid, TraceID: running, InvocationID: invocation, TurnID: turn.ID, Generation: binding.Generation},
		}
		if err := manager.SaveAssistant(t.Context(), message, []agent.ToolRecord{call}); err != nil {
			t.Fatal(err)
		}
		original = manager.View()
	})
	if original.Traces[running].State != "running" || original.Traces[queued].State != "queued" || original.Traces[queued].Hold || len(original.Inputs) != 2 || len(original.Messages) != 2 || len(original.Calls) != 1 {
		t.Fatal("normalization fixture did not retain real running/queued inputs and parent history")
	}
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/snapshot", "/render", "/messages", "/traces/" + running, "/traces/" + queued} {
		if status, _ := call(t, server, token, "GET", "/v1/sessions/"+sid+suffix, "", nil); status != 200 {
			t.Fatalf("read interrupted state status=%d", status)
		}
		assertControlLive(t, catalog, sid, false)
	}
	readOnly, err := catalog.Snapshot(t.Context(), sid)
	if err != nil || readOnly.Traces[running].State != "running" || readOnly.Traces[queued].Hold {
		t.Fatalf("GET normalized interrupted state: %v", err)
	}
	afterGET, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(before, afterGET) || model.Calls() != 0 || calls.Load() != 0 {
		t.Fatal("GET changed interrupted history or executed work")
	}
	status, opened := call(t, server, token, "POST", "/v1/sessions/"+sid+"/open", "", map[string]any{})
	if status != 200 || traceState(opened, running) != "paused" || traceState(opened, queued) != "queued" {
		t.Fatalf("open did not safely normalize interrupted state status=%d", status)
	}
	assertControlLive(t, catalog, sid, true)
	current, err := catalog.Snapshot(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	wantRunning, wantQueued := *original.Traces[running], *original.Traces[queued]
	wantRunning.State, wantQueued.Hold = "paused", true
	if !reflect.DeepEqual(*current.Traces[running], wantRunning) {
		t.Fatal("normalization changed unrelated running trace fields")
	}
	if !reflect.DeepEqual(*current.Traces[queued], wantQueued) {
		t.Fatal("normalization changed unrelated queued trace fields")
	}
	if !reflect.DeepEqual(current.Inputs, original.Inputs) {
		t.Fatal("normalization changed input identity")
	}
	// Snapshot messages use the existing public projection, unlike the raw
	// state fixture. Compare that same projection before and after opening.
	if !reflect.DeepEqual(current.Messages, readOnly.Messages) {
		t.Fatal("normalization changed message identity")
	}
	if !reflect.DeepEqual(current.Calls, original.Calls) {
		t.Fatal("normalization changed call identity")
	}
	if !reflect.DeepEqual(current.Turns, original.Turns) || !reflect.DeepEqual(current.Operations, original.Operations) || len(current.Approvals) != 0 {
		t.Fatal("normalization changed parent turn/operation identity or answered an approval")
	}
	after, err := os.ReadFile(journal)
	if err != nil || !bytes.HasPrefix(after, before) {
		t.Fatal("open rewrote the original workspace header or history")
	}
	// Only two existing safety facts may be appended; map iteration order is
	// unspecified. No new inputs, messages, calls or business operations.
	decoder := json.NewDecoder(bytes.NewReader(after[len(before):]))
	seen := map[string]bool{}
	for {
		var commit store.Commit
		if err := decoder.Decode(&commit); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if len(commit.ControlRecords) != 1 || len(commit.Entries) != 0 || len(commit.BranchUpdates) != 0 || len(commit.Events) != 1 {
			t.Fatal("open appended facts beyond pause and queue hold")
		}
		record := commit.ControlRecords[0]
		if record.Type != "trace" || seen[record.ID] || (record.ID != running && record.ID != queued) {
			t.Fatal("open appended unrelated or duplicate control records")
		}
		wantEvent := "trace.state_changed"
		if record.ID == queued {
			wantEvent = "queue.changed"
		}
		if commit.Events[0].Type != wantEvent || commit.Events[0].Scope.TraceID != record.ID || commit.CommitSeq != original.LastSeq+uint64(len(seen))+1 {
			t.Fatal("open appended unrelated recovery events or revisions")
		}
		seen[record.ID] = true
	}
	if len(seen) != 2 || current.Revision != original.LastSeq+2 || model.Calls() != 0 || calls.Load() != 0 {
		t.Fatal("open did not add exactly two zero-execution normalization commits")
	}
	for range 2 {
		if status, again := call(t, server, token, "POST", "/v1/sessions/"+sid+"/open", "", map[string]any{}); status != 200 || again["revision"] != opened["revision"] {
			t.Fatalf("repeated normalized open status=%d", status)
		}
	}
	repeated, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(after, repeated) || model.Calls() != 0 || calls.Load() != 0 {
		t.Fatal("repeated open rewrote safety facts or executed work")
	}
}

func TestControlOpenRejectsInvalidBodiesAndUnauthorizedAccess(t *testing.T) {
	catalog, server, token, sid, _, model, calls := reopenedControlCatalog(t)
	path := "/v1/sessions/" + sid + "/open"
	for _, body := range []any{nil, json.RawMessage(`null`), []any{}, "{}", true, 1, map[string]any{"workspace": t.TempDir()}, map[string]any{"resume": true}} {
		status, out := call(t, server, token, "POST", path, "", body)
		errBody, _ := out["error"].(map[string]any)
		if status != 400 || errBody["code"] != "invalid_argument" {
			t.Fatalf("invalid open body status=%d %v", status, out)
		}
		assertControlLive(t, catalog, sid, false)
	}
	status, out := call(t, server, rand.Text(), "POST", path, "", map[string]any{})
	errBody, _ := out["error"].(map[string]any)
	if status != 401 || errBody["code"] != "unauthenticated" {
		t.Fatalf("unauthenticated open status=%d %v", status, out)
	}
	status, out = call(t, server, token, "POST", "/v1/sessions/missing/open", "", map[string]any{})
	errBody, _ = out["error"].(map[string]any)
	if status != 404 || errBody["code"] != "not_found" {
		t.Fatalf("unknown session open status=%d %v", status, out)
	}
	assertControlLive(t, catalog, sid, false)
	if err := catalog.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	foreign := server.SessionOptions()
	foreign.Workspace = t.TempDir()
	other, err := NewCatalog(foreign)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	req := httptest.NewRequest("POST", path, strings.NewReader("{}"))
	req = req.WithContext(context.WithValue(req.Context(), principalKey{}, localPrincipal))
	recorder := httptest.NewRecorder()
	newRoutes(other, localPrincipal).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("foreign workspace open status=%d", recorder.Code)
	}
	if model.Calls() != 0 || calls.Load() != 0 {
		t.Fatal("rejected open invoked execution")
	}
}
