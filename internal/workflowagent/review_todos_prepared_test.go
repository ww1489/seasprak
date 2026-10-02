package workflowagent

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
)

func TestWorkflowTodosPreparedArgumentsUseFrozenContent(t *testing.T) {
	const prepared = `{"items":[{"id":"original","state":"pending","title":"prepared title"}]}`
	opts := todoOptions(t, "")
	var preparations atomic.Int32
	opts.Tools[0].PrepareArguments = []func(context.Context, json.RawMessage) (json.RawMessage, error){func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		if preparations.Add(1) != 1 {
			return json.RawMessage(`{"items":[{"id":"original","state":"pending","title":"prepared twice"}]}`), nil
		}
		if !strings.Contains(string(raw), `"original title"`) {
			t.Error("preparation did not receive the original call")
		}
		return json.RawMessage(prepared), nil
	}}
	w := newWorkflow(t, opts)
	submit(t, w)
	final := waitStopped(t, w)
	loaded := loadTodoRun(t, opts)
	calls, frozenRecords := todoRecords(loaded, "workflow_call"), todoRecords(loaded, "workflow_frozen")
	if len(calls) != 1 || len(frozenRecords) != 1 {
		t.Fatalf("original call/frozen descriptor missing: calls=%d frozen=%d state=%s code=%s", len(calls), len(frozenRecords), final.State, final.ErrorCode)
	}
	var call agent.ToolRecord
	var frozen agent.FrozenExecution
	if err := json.Unmarshal(calls[0].Payload, &call); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(frozenRecords[0].Payload, &frozen); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(call.Call.Arguments, `"original title"`) || call.Call.Arguments == prepared || !bytes.Equal(frozen.FinalArguments, []byte(prepared)) || frozen.OriginalArgumentsHash != rawHash([]byte(call.Call.Arguments)) || frozen.FinalArgumentsHash != rawHash(frozen.FinalArguments) || frozen.OriginalArgumentsHash == frozen.FinalArgumentsHash || frozen.CallID != call.Call.CallID {
		t.Fatal("original and final arguments did not retain their independent frozen identities")
	}
	todos, claims := todoRecords(loaded, "workflow_todo"), todoRecords(loaded, "workflow_tool_intent")
	if final.State != "completed" || len(todos) != 1 || len(claims) != 1 || final.Usage.ToolExecutions != 1 || final.Usage.LogicalModelCalls != 0 || final.Usage.TransportRequests != 0 || preparations.Load() != 1 {
		t.Fatalf("prepared TODO did not commit exactly once: state=%s code=%s TODO=%d claims=%d preparations=%d usage=%+v", final.State, final.ErrorCode, len(todos), len(claims), preparations.Load(), final.Usage)
	}
	var update todoUpdate
	if err := json.Unmarshal(todos[0].Payload, &update); err != nil {
		t.Fatal(err)
	}
	if update.CallID != call.Call.CallID || update.InvocationID != call.Scope.InvocationID || update.FrozenHash != frozen.Hash || update.Version != 1 || !bytes.Equal(update.Content, frozen.FinalArguments) {
		t.Fatal("TODO receipt is not bound to the approved final bytes and original call")
	}
	assertPreparedTodoCompleted(t, final, loaded, &preparations)
	assertPreparedTodoCompletedReopen(t, w, opts, final, loaded, &preparations, nil)
}

const preparedTodoContent = `{"items":[{"id":"original","state":"pending","title":"prepared title"}]}`

func preparedTodoOptions(t *testing.T, preparations *atomic.Int32) WorkflowOptions {
	t.Helper()
	opts := todoOptions(t, "once")
	opts.Tools[0].PrepareArguments = []func(context.Context, json.RawMessage) (json.RawMessage, error){func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		if preparations.Add(1) != 1 {
			// A repeated preparation remains schema-valid but must be visible.
			return json.RawMessage(`{"items":[{"id":"original","state":"pending","title":"prepared twice"}]}`), nil
		}
		if !strings.Contains(string(raw), `"original title"`) {
			t.Error("preparation did not receive the original call")
		}
		return json.RawMessage(preparedTodoContent), nil
	}}
	return opts
}

func preparedTodoIdentities(t *testing.T, loaded storage.StoredSession) (agent.ToolRecord, agent.FrozenExecution) {
	t.Helper()
	calls, frozenRecords := todoRecords(loaded, "workflow_call"), todoRecords(loaded, "workflow_frozen")
	if len(calls) != 1 || len(frozenRecords) != 1 {
		t.Fatalf("call/frozen counts=%d/%d, want 1/1", len(calls), len(frozenRecords))
	}
	var call agent.ToolRecord
	var frozen agent.FrozenExecution
	if err := json.Unmarshal(calls[0].Payload, &call); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(frozenRecords[0].Payload, &frozen); err != nil {
		t.Fatal(err)
	}
	hash, err := frozen.Digest()
	if err != nil || hash != frozen.Hash || frozen.Scope != call.Scope || frozen.CallID != call.Call.CallID || !strings.Contains(call.Call.Arguments, `"original title"`) || call.Call.Arguments == preparedTodoContent || !bytes.Equal(frozen.FinalArguments, []byte(preparedTodoContent)) || frozen.OriginalArgumentsHash != rawHash([]byte(call.Call.Arguments)) || frozen.FinalArgumentsHash != rawHash(frozen.FinalArguments) || frozen.OriginalArgumentsHash == frozen.FinalArgumentsHash {
		t.Fatal("original A and frozen B lost their independent bytes, hashes or scope")
	}
	return call, frozen
}

func assertPreparedTodoCompleted(t *testing.T, final WorkflowSnapshot, loaded storage.StoredSession, preparations *atomic.Int32) {
	t.Helper()
	call, frozen := preparedTodoIdentities(t, loaded)
	todos, claims, observations := todoRecords(loaded, "workflow_todo"), todoRecords(loaded, "workflow_tool_intent"), todoRecords(loaded, "workflow_tool_observation")
	if final.State != "completed" || final.ErrorCode != "" || len(todos) != 1 || len(claims) != 1 || len(observations) != 1 || final.Usage.ToolExecutions != 1 || final.Usage.LogicalModelCalls != 0 || final.Usage.TransportRequests != 0 || preparations.Load() != 1 {
		t.Fatalf("prepared TODO did not commit exactly once: state=%s code=%s TODO=%d claims=%d observations=%d preparations=%d usage=%+v", final.State, final.ErrorCode, len(todos), len(claims), len(observations), preparations.Load(), final.Usage)
	}
	var update todoUpdate
	var claim toolIntent
	var observation agent.ToolRecord
	if err := json.Unmarshal(todos[0].Payload, &update); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(claims[0].Payload, &claim); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(observations[0].Payload, &observation); err != nil {
		t.Fatal(err)
	}
	if todos[0].ID != call.Call.CallID || update.CallID != call.Call.CallID || update.InvocationID != call.Scope.InvocationID || update.FrozenHash != frozen.Hash || update.Version != 1 || !bytes.Equal(update.Content, frozen.FinalArguments) || claim.Call != call.Call || claim.Scope != call.Scope || observation.Call != call.Call || !observation.Claimed || observation.Observation == nil || observation.Observation.Status != "succeeded" || observation.Observation.SideEffect != "confirmed" || !observation.Observation.Executed || observation.Observation.Content != preparedTodoContent {
		t.Fatal("claim, receipt or confirmed result changed original A or final B")
	}
	claimed := false
	for _, c := range loaded.Commits {
		for _, r := range c.ControlRecords {
			if r.Type == "workflow_tool_intent" {
				claimed = true
			}
			if r.Type == "workflow_todo" && !claimed {
				t.Fatal("TODO committed before its claim")
			}
		}
	}
	var result map[string]string
	if err := json.Unmarshal(final.Result, &result); err != nil || result["result"] != preparedTodoContent {
		t.Fatalf("completed output is not frozen B: %s", final.Result)
	}
	if _, err := replay(loaded, loaded.Header.RunID); err != nil {
		t.Fatal(err)
	}
}

func assertPreparedTodoNoEffects(t *testing.T, w *WorkflowAgent, opts WorkflowOptions, preparations *atomic.Int32, revision uint64) WorkflowSnapshot {
	t.Helper()
	snapshot, err := w.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	loaded := loadTodoRun(t, opts)
	preparedTodoIdentities(t, loaded)
	if snapshot.State != "paused" || !snapshot.ExecutionStopped || snapshot.Revision != revision || snapshot.Usage.ToolExecutions != 0 || snapshot.Usage.LogicalModelCalls != 0 || snapshot.Usage.TransportRequests != 0 || preparations.Load() != 1 || len(todoRecords(loaded, "workflow_todo")) != 0 || len(todoRecords(loaded, "workflow_tool_intent")) != 0 || len(todoRecords(loaded, "workflow_tool_observation")) != 0 {
		t.Fatal("approval, Answer or Open executed, changed revision or repeated preparation")
	}
	return snapshot
}

func assertPreparedTodoCompletedReopen(t *testing.T, w *WorkflowAgent, opts WorkflowOptions, final WorkflowSnapshot, loaded storage.StoredSession, preparations *atomic.Int32, resume *WorkflowOperationReceipt) {
	t.Helper()
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, readOnly := range []bool{true, false} {
		opts.ReadOnly = readOnly
		reopened, err := OpenWorkflowAgent(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(context.Background())
		after, err := reopened.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after.State != final.State || after.Revision != final.Revision || after.Usage != final.Usage || !bytes.Equal(after.Result, final.Result) || after.BindingVersion != final.BindingVersion || after.InstanceID == final.InstanceID {
			t.Fatal("disk Open changed the completed result, usage or binding")
		}
		if !readOnly {
			input, err := reopened.SubmitInput(t.Context(), WorkflowInputCommand{Input: json.RawMessage(`{}`), Principal: "local", IdempotencyKey: "input"})
			if err != nil {
				t.Fatal(err)
			}
			state, err := replay(loaded, opts.RunID)
			if err != nil || state.Input == nil || input != state.Input.Receipt {
				t.Fatal("original input key changed its acceptance receipt")
			}
			if resume != nil {
				again, err := reopened.Resume(t.Context(), WorkflowControlCommand{IdempotencyKey: "prepared-resume", Principal: "local"})
				if err != nil || again != *resume {
					t.Fatal("original resume key changed its acceptance receipt")
				}
			}
		}
		if !reflect.DeepEqual(loadTodoRun(t, opts), loaded) || preparations.Load() != 1 {
			t.Fatal("readonly/writable Open or duplicate controls changed journal or prepared again")
		}
		if err := reopened.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkflowTodosPreparedApprovalUsesOriginalFrozenBytes(t *testing.T) {
	var preparations atomic.Int32
	opts := preparedTodoOptions(t, &preparations)
	w := newWorkflow(t, opts)
	submit(t, w)
	paused := waitStopped(t, w)
	assertPreparedTodoNoEffects(t, w, opts, &preparations, paused.Revision)
	before := loadTodoRun(t, opts)
	answerTodo(t, w, paused, "allowed-once")
	assertPreparedTodoNoEffects(t, w, opts, &preparations, paused.Revision)
	if !reflect.DeepEqual(loadTodoRun(t, opts), before) {
		t.Fatal("Answer wrote durable bytes")
	}
	resume, err := w.Resume(t.Context(), WorkflowControlCommand{IdempotencyKey: "prepared-resume", Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	final := waitStopped(t, w)
	loaded := loadTodoRun(t, opts)
	assertPreparedTodoCompleted(t, final, loaded, &preparations)
	if !reflect.DeepEqual(todoRecords(loaded, "workflow_call"), todoRecords(before, "workflow_call")) || !reflect.DeepEqual(todoRecords(loaded, "workflow_frozen"), todoRecords(before, "workflow_frozen")) {
		t.Fatal("Resume rewrote the original call or frozen descriptor")
	}
	assertPreparedTodoCompletedReopen(t, w, opts, final, loaded, &preparations, &resume)
}

func TestWorkflowTodosPreparedDiskReopenRequiresFreshAnswer(t *testing.T) {
	var preparations atomic.Int32
	opts := preparedTodoOptions(t, &preparations)
	w := newWorkflow(t, opts)
	submit(t, w)
	paused := waitStopped(t, w)
	assertPreparedTodoNoEffects(t, w, opts, &preparations, paused.Revision)
	var question WorkflowInteraction
	for _, question = range paused.Interactions {
	}
	answerTodo(t, w, paused, "allowed-once")
	assertPreparedTodoNoEffects(t, w, opts, &preparations, paused.Revision)
	before := loadTodoRun(t, opts)
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	after := assertPreparedTodoNoEffects(t, reopened, opts, &preparations, paused.Revision)
	if after.InstanceID == paused.InstanceID || len(after.Interactions) != 0 || !reflect.DeepEqual(loadTodoRun(t, opts), before) {
		t.Fatal("Open inherited approval, prepared again or changed durable bytes")
	}
	_, err = reopened.RespondInteraction(t.Context(), WorkflowInteractionResponse{InteractionID: question.ID, Decision: "allowed-once", ExpectedRevision: after.Revision, Principal: "local"})
	requireCode(t, err, product.CodeNotFound)
	if _, err := reopened.Resume(t.Context(), WorkflowControlCommand{IdempotencyKey: "ask-again", Principal: "local"}); err != nil {
		t.Fatal(err)
	}
	askedAgain := waitStopped(t, reopened)
	assertPreparedTodoNoEffects(t, reopened, opts, &preparations, askedAgain.Revision)
	var fresh WorkflowInteraction
	for _, fresh = range askedAgain.Interactions {
	}
	if len(askedAgain.Interactions) != 1 || fresh.ID == question.ID || fresh.ToolCallID != question.ToolCallID || fresh.InstanceID != after.InstanceID {
		t.Fatal("new instance did not request fresh authority for the original call")
	}
	answerTodo(t, reopened, askedAgain, "allowed-once")
	assertPreparedTodoNoEffects(t, reopened, opts, &preparations, askedAgain.Revision)
	resume, err := reopened.Resume(t.Context(), WorkflowControlCommand{IdempotencyKey: "prepared-resume", Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	final := waitStopped(t, reopened)
	loaded := loadTodoRun(t, opts)
	assertPreparedTodoCompleted(t, final, loaded, &preparations)
	if !reflect.DeepEqual(todoRecords(loaded, "workflow_call"), todoRecords(before, "workflow_call")) || !reflect.DeepEqual(todoRecords(loaded, "workflow_frozen"), todoRecords(before, "workflow_frozen")) {
		t.Fatal("disk recovery rewrote original A or prepared B again")
	}
	assertPreparedTodoCompletedReopen(t, reopened, opts, final, loaded, &preparations, &resume)
}

func TestWorkflowTodosPreparedReplayRequiresExactFinalBytes(t *testing.T) {
	var preparations atomic.Int32
	opts := preparedTodoOptions(t, &preparations)
	opts.Tools[0].Execution.RequestedGrantRef = ""
	w := newWorkflow(t, opts)
	submit(t, w)
	final := waitStopped(t, w)
	loaded := loadTodoRun(t, opts)
	assertPreparedTodoCompleted(t, final, loaded, &preparations)
	call, _ := preparedTodoIdentities(t, loaded)
	for _, mutation := range []string{"original_A", "final_B_whitespace", "frozen_hash"} {
		t.Run(mutation, func(t *testing.T) {
			forged := storage.CloneSession(loaded)
			changed := 0
			for i := range forged.Commits {
				for j := range forged.Commits[i].ControlRecords {
					r := &forged.Commits[i].ControlRecords[j]
					if r.Type != "workflow_todo" {
						continue
					}
					var update todoUpdate
					if err := json.Unmarshal(r.Payload, &update); err != nil {
						t.Fatal(err)
					}
					switch mutation {
					case "original_A":
						update.Content = json.RawMessage(call.Call.Arguments)
						*r = record(r.Type, r.ID, update)
					case "final_B_whitespace":
						// Marshal compacts RawMessage; insert the byte in the record
						// itself to preserve a valid but byte-distinct final B.
						r.Payload = bytes.Replace(r.Payload, []byte(`"content":{"items":`), []byte(`"content":{ "items":`), 1)
					case "frozen_hash":
						update.FrozenHash = "foreign-frozen-hash"
						*r = record(r.Type, r.ID, update)
					}
					if !json.Valid(r.Payload) || bytes.Equal(r.Payload, todoRecords(loaded, "workflow_todo")[0].Payload) {
						t.Fatal("mutation did not produce valid, changed record bytes")
					}
					changed++
				}
			}
			if changed != 1 {
				t.Fatal("mutation did not target the unique receipt")
			}
			_, err := replay(forged, opts.RunID)
			requireCode(t, err, product.CodeIncompatibleVersion)
		})
	}
	if !reflect.DeepEqual(loadTodoRun(t, opts), loaded) || preparations.Load() != 1 {
		t.Fatal("negative replay changed the original journal or reexecuted preparation")
	}
}
