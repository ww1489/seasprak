package workflowagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

type evidenceStore struct {
	storage.Store
	loaded  storage.StoredSession
	appends atomic.Int32
}

func (s *evidenceStore) Load(context.Context, string) (storage.StoredSession, error) {
	return storage.CloneSession(s.loaded), nil
}
func (s *evidenceStore) Append(context.Context, string, storage.ExpectedCommit, storage.Commit) (storage.CommitReceipt, error) {
	s.appends.Add(1)
	return storage.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "unexpected replay write")
}
func (*evidenceStore) Close() error { return nil }

func rejectEvidence(t *testing.T, opts WorkflowOptions, loaded storage.StoredSession) {
	t.Helper()
	_, err := replay(loaded, opts.RunID)
	requireCode(t, err, product.CodeIncompatibleVersion)
	probe := &evidenceStore{Store: opts.Store, loaded: loaded}
	opts.Store, opts.ReadOnly = probe, true
	opened, err := OpenWorkflowAgent(t.Context(), opts)
	if opened != nil {
		opened.Close(context.Background())
		t.Fatal("invalid evidence opened")
	}
	requireCode(t, err, product.CodeIncompatibleVersion)
	if probe.appends.Load() != 0 {
		t.Fatal("replay attempted a write")
	}
}

func TestWorkflowReplayRejectsWrongTransportAndPerCallOccupancy(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "observed"}[observed], func(t *testing.T) {
			var effects, wires atomic.Int32
			legacy := testkit.NewFake(testkit.Step{Text: "complete"})
			m := &observedModel{}
			m.client = &http.Client{Transport: llm.NewObservedTransport(offlineWire(func(r *http.Request) (*http.Response, error) {
				wires.Add(1)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":2,"completion_tokens":1}}`)), Request: r}, nil
			}), llm.UsageCollection{Protocol: "openai-chat", MaxBytes: 4096})}
			opts := testOptions(t, modelThenTool(), legacy, &effects)
			if observed {
				opts.Models["chosen"] = m
			}
			f := injectStore(t, &opts)
			w := newWorkflow(t, opts)
			submit(t, w)
			s := waitStopped(t, w)
			if s.State != "completed" {
				t.Fatalf("baseline failed: %s %s", s.State, s.ErrorCode)
			}
			loaded, err := f.Load(t.Context(), opts.RunID)
			if err != nil {
				t.Fatal(err)
			}
			mutations := []string{"per_call", "clear_logical", "started_removed"}
			if observed {
				mutations = append(mutations, "attempt", "node", "purpose", "ordinal", "details_attempt", "details_ordinal", "negative_usage")
			}
			for _, mutation := range mutations {
				t.Run(mutation, func(t *testing.T) {
					forged := storage.CloneSession(loaded)
					changed := false
					for i := range forged.Commits {
						var kept []storage.Record
						for _, r := range forged.Commits[i].ControlRecords {
							if mutation == "started_removed" && r.Type == "workflow_model_attempt" {
								var p attemptRecord
								json.Unmarshal(r.Payload, &p)
								if p.Status == "started" {
									changed = true
									continue
								}
							}
							if r.Type == "workflow_budget" && !changed {
								var p budgetRecord
								json.Unmarshal(r.Payload, &p)
								if p.Local.TransportRequests == 1 && !strings.HasPrefix(mutation, "details_") && mutation != "negative_usage" && mutation != "started_removed" {
									switch mutation {
									case "per_call":
										p.Local.ModelRequests = 0
									case "clear_logical":
										p.Local.ModelCallID = ""
									case "attempt":
										p.Local.LastTransport.AttemptID = "another-attempt"
									case "node":
										p.Local.LastTransport.ModelCallID = "another-node"
									case "purpose":
										p.Local.LastTransport.Purpose = "agent"
									case "ordinal":
										p.Local.LastTransport.TransportAttempt = 2
									}
									r = record(r.Type, r.ID, p)
									changed = true
									// This valid prefix must already reject the forged request,
									// independently of any later completion consistency check.
									forged.Commits = forged.Commits[:i+1]
								}
							}
							if r.Type == "workflow_model_attempt" && !changed {
								var p attemptRecord
								json.Unmarshal(r.Payload, &p)
								if p.Status == "complete" && len(p.Result.Details.Usage) > 0 {
									switch mutation {
									case "details_attempt":
										p.Result.Details.Usage[0].Request.AttemptID = "another-attempt"
										changed = true
									case "details_ordinal":
										p.Result.Details.Usage[0].Request.TransportAttempt = 3
										changed = true
									case "negative_usage":
										p.Result.Details.Usage[0].Snapshot.Usage.InputTotal.Value = -1
										changed = true
									}
									if changed {
										r = record(r.Type, r.ID, p)
									}
								}
							}
							kept = append(kept, r)
						}
						forged.Commits[i].ControlRecords = kept
						if i+1 == len(forged.Commits) {
							break
						}
					}
					if !changed {
						t.Fatal("mutation did not touch actual evidence")
					}
					rejectEvidence(t, opts, forged)
				})
			}
			if effects.Load() != 1 || observed && (m.entered.Load() != 1 || wires.Load() != 2) || !observed && legacy.Calls() != 1 {
				t.Fatal("replay repeated an effect")
			}
		})
	}
}

func TestWorkflowRootResultCanonicalStructuredProjection(t *testing.T) {
	for _, kind := range []string{"input", "literal", "subflow"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			definition := toolOnly()
			definition.InputSchema = json.RawMessage(`{"type":"object","properties":{"value":{"type":"object"}},"required":["value"],"additionalProperties":false}`)
			definition.Nodes = []WorkflowNode{{ID: "s", Type: "start"}, {ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"value": output("s", "value")}}}
			definition.Edges = []WorkflowEdge{{From: "s", To: "e"}}
			value := json.RawMessage(`{ "z": [3.0, {"b": 2, "a": 1}], "a": 9007199254740993 }`)
			if kind == "literal" {
				definition.Nodes = []WorkflowNode{{ID: "s", Type: "start"}, {ID: "lit", Type: "literal", Inputs: map[string]WorkflowValue{"value": {Literal: value}}}, {ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"value": output("lit", "value")}}}
				definition.Edges = []WorkflowEdge{{From: "s", To: "lit"}, {From: "lit", To: "e"}}
			}
			opts := testOptions(t, definition, nil, &effects)
			if kind == "subflow" {
				child := definition
				child.Name = "child"
				opts.Subflows = map[string]WorkflowDefinition{"child@v1": child}
				opts.Definition.Nodes = []WorkflowNode{{ID: "s", Type: "start"}, {ID: "sub", Type: "subflow", Subflow: "child@v1", Inputs: map[string]WorkflowValue{"value": output("s", "value")}}, {ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"value": output("sub", "value")}}}
				opts.Definition.Edges = []WorkflowEdge{{From: "s", To: "sub"}, {From: "sub", To: "e"}}
			}
			backend := injectStore(t, &opts)
			w := newWorkflow(t, opts)
			if _, err := w.SubmitInput(t.Context(), WorkflowInputCommand{Input: json.RawMessage(`{"value":` + string(value) + `}`), Principal: "local", IdempotencyKey: "input"}); err != nil {
				t.Fatal(err)
			}
			completed := waitStopped(t, w)
			want := `{"value":{"a":9007199254740993,"z":[3.0,{"a":1,"b":2}]}}`
			if completed.State != "completed" || string(completed.Result) != want || completed.Usage != (agent.Usage{}) || effects.Load() != 0 {
				t.Fatalf("valid structured root changed: state=%s code=%s result=%s usage=%+v effects=%d", completed.State, completed.ErrorCode, completed.Result, completed.Usage, effects.Load())
			}
			loaded, err := backend.Load(t.Context(), opts.RunID)
			if err != nil {
				t.Fatal(err)
			}
			probe := &evidenceStore{Store: backend, loaded: loaded}
			opts.Store, opts.ReadOnly = probe, true
			opened, err := OpenWorkflowAgent(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close(context.Background())
			after, err := opened.Snapshot(t.Context())
			if err != nil || string(after.Result) != want || after.Revision != completed.Revision || probe.appends.Load() != 0 || effects.Load() != 0 {
				t.Fatalf("read-only root replay changed result or effects: err=%v result=%s", err, after.Result)
			}
		})
	}
}

func TestWorkflowReplayRejectsCompletedResultMismatch(t *testing.T) {
	for _, kind := range []string{"model", "tool", "subflow"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			m := testkit.NewFake(testkit.Step{Text: "original-model"})
			opts := testOptions(t, modelThenTool(), m, &effects)
			if kind == "subflow" {
				child := toolOnly()
				child.Name = "child"
				d := child
				d.Name = "parent"
				d.Nodes = []WorkflowNode{{ID: "s", Type: "start"}, {ID: "sub", Type: "subflow", Subflow: "child@v1"}, {ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"result": output("sub", "result")}}}
				d.Edges = []WorkflowEdge{{From: "s", To: "sub"}, {From: "sub", To: "e"}}
				opts.Definition = d
				opts.Subflows = map[string]WorkflowDefinition{"child@v1": child}
			}
			f := injectStore(t, &opts)
			w := newWorkflow(t, opts)
			submit(t, w)
			if s := waitStopped(t, w); s.State != "completed" {
				t.Fatalf("baseline %s %s", s.State, s.ErrorCode)
			}
			loaded, err := f.Load(t.Context(), opts.RunID)
			if err != nil {
				t.Fatal(err)
			}
			changed := false
			for i := range loaded.Commits {
				for j, r := range loaded.Commits[i].ControlRecords {
					if r.Type != "workflow_node" {
						continue
					}
					var p nodeRecord
					json.Unmarshal(r.Payload, &p)
					if p.Node.Kind == kind && p.Node.State == "completed" {
						p.Result = "forged-result"
						if kind == "subflow" {
							p.Result = `{"result":"forged-result"}`
						}
						loaded.Commits[i].ControlRecords[j] = record(r.Type, r.ID, p)
						changed = true
					}
				}
			}
			if !changed {
				t.Fatal("no completed result mutated")
			}
			rejectEvidence(t, opts, loaded)
			if effects.Load() != 1 || kind != "subflow" && m.Calls() != 1 || kind == "subflow" && m.Calls() != 0 {
				t.Fatal("replay executed a node")
			}
		})
	}
}
