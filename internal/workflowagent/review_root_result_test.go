package workflowagent

import (
	"bytes"
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

// This injected backend deliberately retains its Load result to exercise the
// runtime's incoming ownership boundary, independently of built-in backends.
type reviewLoadedStore struct {
	storage.Store
	loaded  storage.StoredSession
	appends atomic.Int32
}

func (s *reviewLoadedStore) Load(context.Context, string) (storage.StoredSession, error) {
	return s.loaded, nil
}
func (s *reviewLoadedStore) Append(ctx context.Context, id string, expected storage.ExpectedCommit, c storage.Commit) (storage.CommitReceipt, error) {
	s.appends.Add(1)
	return s.Store.Append(ctx, id, expected, c)
}
func (*reviewLoadedStore) Close() error { return nil }

func TestWorkflowReplayRejectsRootResultMismatch(t *testing.T) {
	for _, kind := range []string{"input", "tool", "model"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			m := testkit.NewFake(testkit.Step{Text: "original-model-result"})
			def := toolOnly()
			if kind == "model" {
				def = modelThenTool()
			}
			if kind == "input" {
				def.Nodes = []WorkflowNode{{ID: "s", Type: "start"}, {ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"name": output("s", "name")}}}
				def.InputSchema = json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`)
				def.Edges = []WorkflowEdge{{From: "s", To: "e"}}
			}
			opts := testOptions(t, def, m, &effects)
			f := injectStore(t, &opts)
			w := newWorkflow(t, opts)
			input := json.RawMessage(`{}`)
			if kind == "input" {
				input = json.RawMessage(`{"name":"original-input"}`)
			}
			if _, err := w.SubmitInput(t.Context(), WorkflowInputCommand{Input: input, Principal: "local", IdempotencyKey: "input"}); err != nil {
				t.Fatal(err)
			}
			completed := waitStopped(t, w)
			if completed.State != "completed" {
				t.Fatalf("fixture did not complete: state=%s code=%s", completed.State, completed.ErrorCode)
			}
			loaded, err := f.Load(t.Context(), opts.RunID)
			if err != nil {
				t.Fatal(err)
			}
			beforeModel, beforeEffects := m.Calls(), effects.Load()
			openOpts := opts
			openOpts.ReadOnly = true
			validStore := &reviewLoadedStore{Store: f, loaded: storage.CloneSession(loaded)}
			openOpts.Store = validStore
			valid, err := OpenWorkflowAgent(t.Context(), openOpts)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := valid.Snapshot(t.Context())
			if err != nil || !bytes.Equal(snapshot.Result, completed.Result) {
				t.Fatalf("valid root result changed: err=%v", err)
			}
			if err := valid.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, replacement := range []json.RawMessage{json.RawMessage(`{"name":"forged","result":"forged"}`), json.RawMessage(`[]`)} {
				forged := storage.CloneSession(loaded)
				changed := 0
				for i := range forged.Commits {
					for j, r := range forged.Commits[i].ControlRecords {
						if r.Type != "workflow_run" {
							continue
						}
						var run runRecord
						if err := json.Unmarshal(r.Payload, &run); err != nil {
							t.Fatal(err)
						}
						if run.State == "completed" {
							run.Result = replacement
							forged.Commits[i].ControlRecords[j] = record(r.Type, r.ID, run)
							changed++
						}
					}
				}
				if changed != 1 {
					t.Fatalf("changed %d completed facts, want 1", changed)
				}
				badStore := &reviewLoadedStore{Store: f, loaded: forged}
				openOpts.Store = badStore
				opened, err := OpenWorkflowAgent(t.Context(), openOpts)
				if opened != nil {
					opened.Close(t.Context())
					t.Errorf("Open accepted forged root result %s", replacement)
				}
				if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
					t.Errorf("Open error=%v, want incompatible_version", err)
				}
				if badStore.appends.Load() != 0 || m.Calls() != beforeModel || effects.Load() != beforeEffects {
					t.Fatal("rejected replay appended or repeated effects")
				}
			}
		})
	}
}

func TestWorkflowSubflowCanonicalLiteralResults(t *testing.T) {
	for _, kind := range []string{"end_literal", "literal_node", "parent_literal_input"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			value := json.RawMessage(`{"z":[3.0,{"b":2,"a":1}],"a":9007199254740993}`)
			child := toolOnly()
			child.Name = "canonical-child"
			child.InputSchema = json.RawMessage(`{"type":"object","properties":{"value":{"type":"object"}},"required":["value"],"additionalProperties":false}`)
			child.Nodes = []WorkflowNode{{ID: "s", Type: "start"}, {ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"value": {Literal: value}}}}
			child.Edges = []WorkflowEdge{{From: "s", To: "e"}}
			if kind == "literal_node" {
				child.Nodes = []WorkflowNode{{ID: "s", Type: "start"}, {ID: "lit", Type: "literal", Inputs: map[string]WorkflowValue{"value": {Literal: value}}}, {ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"value": output("lit", "value")}}}
				child.Edges = []WorkflowEdge{{From: "s", To: "lit"}, {From: "lit", To: "e"}}
			}
			if kind == "parent_literal_input" {
				child.Nodes[1].Inputs = map[string]WorkflowValue{"value": output("s", "value")}
			}
			root := toolOnly()
			root.Name, root.InputSchema = "canonical-parent", child.InputSchema
			root.Nodes = []WorkflowNode{{ID: "s", Type: "start"}, {ID: "sub", Type: "subflow", Subflow: "canonical-child@v1", Inputs: map[string]WorkflowValue{"value": output("s", "value")}}, {ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"value": output("sub", "value")}}}
			root.Edges = []WorkflowEdge{{From: "s", To: "sub"}, {From: "sub", To: "e"}}
			if kind == "parent_literal_input" {
				root.Nodes[1].Inputs = map[string]WorkflowValue{"value": {Literal: value}}
			}
			opts := testOptions(t, root, nil, &effects)
			opts.Subflows = map[string]WorkflowDefinition{"canonical-child@v1": child}
			backend := injectStore(t, &opts)
			w := newWorkflow(t, opts)
			if _, err := w.SubmitInput(t.Context(), WorkflowInputCommand{Input: json.RawMessage(`{"value":` + string(value) + `}`), Principal: "local", IdempotencyKey: "input"}); err != nil {
				t.Fatal(err)
			}
			completed := waitStopped(t, w)
			want := `{"value":{"a":9007199254740993,"z":[3.0,{"a":1,"b":2}]}}`
			if completed.State != "completed" || string(completed.Result) != want || completed.Usage != (agent.Usage{}) || effects.Load() != 0 {
				t.Fatalf("legal child literal rejected: state=%s code=%s result=%s effects=%d", completed.State, completed.ErrorCode, completed.Result, effects.Load())
			}
			for _, node := range completed.WorkflowNodes {
				if node.Kind == "subflow" && node.State != "completed" {
					t.Fatal("child invocation did not complete")
				}
			}
			loaded, err := backend.Load(t.Context(), opts.RunID)
			if err != nil {
				t.Fatal(err)
			}
			probe := &reviewLoadedStore{Store: backend, loaded: loaded}
			opts.ReadOnly, opts.Store = true, probe
			opened, err := OpenWorkflowAgent(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close(context.Background())
			after, err := opened.Snapshot(t.Context())
			if err != nil || string(after.Result) != want || after.Revision != completed.Revision || after.Usage != completed.Usage || probe.appends.Load() != 0 || effects.Load() != 0 {
				t.Fatalf("child replay changed result or effects: err=%v", err)
			}
		})
	}
}

func TestWorkflowReplayOwnsLoadedHistoricalEvents(t *testing.T) {
	var effects atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &effects)
	f := injectStore(t, &opts)
	w := newWorkflow(t, opts)
	submit(t, w)
	completed := waitStopped(t, w)
	loaded, err := f.Load(t.Context(), opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var original []agent.Event
	for _, c := range loaded.Commits {
		for _, ev := range c.Events {
			original = append(original, cloneEvent(ev))
		}
	}
	if len(original) == 0 || effects.Load() != 1 {
		t.Fatal("fixture lacks real historical events or effect")
	}
	retained := &reviewLoadedStore{Store: f, loaded: loaded}
	openOpts := opts
	openOpts.ReadOnly, openOpts.Store = true, retained
	opened, err := OpenWorkflowAgent(t.Context(), openOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close(context.Background())
	for i := range retained.loaded.Commits {
		for j := range retained.loaded.Commits[i].Events {
			ev := &retained.loaded.Commits[i].Events[j]
			*ev.DurableSeq += completed.DurableSeq + 1
			if len(ev.Payload) > 2 {
				ev.Payload[2] = 'X'
			}
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	sub, err := opened.SubscribeFrom(ctx, WorkflowSubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	i := 0
	for ev := range sub.Events {
		if i >= len(original) {
			t.Fatal("replay produced extra history")
		}
		want := original[i]
		if ev.DurableSeq == nil || *ev.DurableSeq != *want.DurableSeq || !bytes.Equal(ev.Payload, want.Payload) || ev.Type != want.Type {
			t.Errorf("backend mutation changed historical event %d", i)
		}
		i++
	}
	if i != len(original) {
		t.Errorf("backend mutation changed delivered history count=%d, want %d", i, len(original))
	}
	if ctx.Err() != nil || sub.Err() != nil {
		t.Fatalf("history did not end normally: ctx=%v subscription=%v", ctx.Err(), sub.Err())
	}
	after, err := opened.Snapshot(t.Context())
	if err != nil || after.Revision != completed.Revision || after.DurableSeq != completed.DurableSeq || retained.appends.Load() != 0 || effects.Load() != 1 {
		t.Fatalf("read-only ownership changed revision or effects: err=%v", err)
	}
}
