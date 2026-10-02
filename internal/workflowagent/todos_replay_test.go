package workflowagent

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
)

func TestWorkflowTodosReplayRejectsAlteredOrOutOfOrderReceipts(t *testing.T) {
	opts := todoOptions(t, "")
	w := newWorkflow(t, opts)
	submit(t, w)
	final := waitStopped(t, w)
	loaded := loadTodoRun(t, opts)
	if final.State != "completed" {
		t.Fatal("real TODO fixture failed")
	}
	if _, err := replay(loaded, opts.RunID); err != nil {
		t.Fatal(err)
	}
	original := todoRecords(loaded, "workflow_todo")[0]
	for _, mutation := range []string{"duplicate", "effect_before_claim", "effect_after_observation", "call", "invocation", "frozen_hash", "version", "content", "content_whitespace", "receipt_id", "unknown_field"} {
		t.Run(mutation, func(t *testing.T) {
			forged := storage.CloneSession(loaded)
			for i := range forged.Commits {
				var kept []storage.Record
				for _, r := range forged.Commits[i].ControlRecords {
					if r.Type == "workflow_todo" {
						if mutation == "effect_before_claim" || mutation == "effect_after_observation" {
							continue
						}
						var u todoUpdate
						if err := json.Unmarshal(r.Payload, &u); err != nil {
							t.Fatal(err)
						}
						switch mutation {
						case "call":
							u.CallID = "foreign-call"
						case "invocation":
							u.InvocationID = "foreign-invocation"
						case "frozen_hash":
							u.FrozenHash = "changed-frozen"
						case "version":
							u.Version = 2
						case "content":
							u.Content = json.RawMessage(`{"items":[]}`)
						}
						r = record(r.Type, r.ID, u)
						switch mutation {
						case "receipt_id":
							r.ID = "foreign-receipt"
						case "content_whitespace":
							r.Payload = bytes.Replace(r.Payload, []byte(`"content":{"items":`), []byte(`"content":{ "items":`), 1)
						case "unknown_field":
							r.Payload = append(r.Payload[:len(r.Payload)-1], []byte(`,"extra":true}`)...)
						case "duplicate":
							kept = append(kept, original)
						}
					}
					if mutation == "effect_before_claim" && r.Type == "workflow_tool_intent" {
						kept = append(kept, original)
					}
					kept = append(kept, r)
					if mutation == "effect_after_observation" && r.Type == "workflow_tool_observation" {
						kept = append(kept, original)
					}
				}
				forged.Commits[i].ControlRecords = kept
			}
			_, err := replay(forged, opts.RunID)
			requireCode(t, err, product.CodeIncompatibleVersion)
		})
	}
	if len(todoRecords(loadTodoRun(t, opts), "workflow_todo")) != 1 || len(todoRecords(loadTodoRun(t, opts), "workflow_tool_intent")) != 1 {
		t.Fatal("replay executed or changed original records")
	}
}

func TestWorkflowTodosReplayRejectsMissingOwnershipTag(t *testing.T) {
	opts := todoOptions(t, "")
	w := newWorkflow(t, opts)
	loaded := loadTodoRun(t, opts)
	var initial initialRecord
	if err := json.Unmarshal(loaded.Commits[0].ControlRecords[0].Payload, &initial); err != nil {
		t.Fatal(err)
	}
	initial.Manifest.TodoBackend = ""
	initial.BindingVersion = digest(initial.Manifest)
	loaded.Commits[0].ControlRecords[0] = record("workflow_initialized", "initial", initial)
	_, err := replay(loaded, opts.RunID)
	requireCode(t, err, product.CodeIncompatibleVersion)
	if len(todoRecords(loadTodoRun(t, opts), "workflow_todo")) != 0 {
		t.Fatal("legacy ownership probe executed")
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowTodosReplayRequiresMatchingEffectBeforeSuccess(t *testing.T) {
	opts := todoOptions(t, "")
	w := newWorkflow(t, opts)
	submit(t, w)
	final := waitStopped(t, w)
	loaded := loadTodoRun(t, opts)
	if final.State != "completed" || len(todoRecords(loaded, "workflow_todo")) != 1 {
		t.Fatal("real TODO fixture did not commit")
	}
	for _, mutation := range []string{"missing_effect", "none_after_effect"} {
		t.Run(mutation, func(t *testing.T) {
			forged := storage.CloneSession(loaded)
			for i := range forged.Commits {
				var records []storage.Record
				for _, r := range forged.Commits[i].ControlRecords {
					if mutation == "missing_effect" && r.Type == "workflow_todo" {
						continue
					}
					if mutation == "none_after_effect" && r.Type == "workflow_tool_observation" {
						var call agent.ToolRecord
						if err := json.Unmarshal(r.Payload, &call); err != nil {
							t.Fatal(err)
						}
						call.Observation.SideEffect = "none"
						call.Observation.Executed = false
						r = record(r.Type, r.ID, call)
					}
					records = append(records, r)
				}
				forged.Commits[i].ControlRecords = records
			}
			_, err := replay(forged, opts.RunID)
			requireCode(t, err, product.CodeIncompatibleVersion)
		})
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
