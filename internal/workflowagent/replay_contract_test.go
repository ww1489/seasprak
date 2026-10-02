package workflowagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestWorkflowReplayRejectsCompleteAttemptWithInvalidResponse(t *testing.T) {
	var tool atomic.Int32
	opts := testOptions(t, modelThenTool(), testkit.NewFake(testkit.Step{Text: "complete"}), &tool)
	f := injectStore(t, &opts)
	w := newWorkflow(t, opts)
	submit(t, w)
	waitStopped(t, w)
	loaded, err := f.Load(t.Context(), opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"wrong_role", "missing_finish", "unexpected_tool"} {
		t.Run(mutation, func(t *testing.T) {
			forged := storage.CloneSession(loaded)
			for i := range forged.Commits {
				for j, r := range forged.Commits[i].ControlRecords {
					if r.Type == "workflow_model_attempt" {
						var p attemptRecord
						json.Unmarshal(r.Payload, &p)
						if p.Status != "complete" {
							continue
						}
						switch mutation {
						case "wrong_role":
							p.Result.Message.Role = schema.AgenticRoleTypeUser
						case "missing_finish":
							delete(p.Result.Message.Extra, "seasprak.finish")
						case "unexpected_tool":
							p.Result.Message.ContentBlocks = append(p.Result.Message.ContentBlocks, schema.NewContentBlock(&schema.FunctionToolCall{CallID: "forged", Name: "echo", Arguments: `{}`}))
						}
						forged.Commits[i].ControlRecords[j] = record(r.Type, r.ID, p)
					}
				}
			}
			_, err := replay(forged, opts.RunID)
			requireCode(t, err, product.CodeIncompatibleVersion)
		})
	}
}
func TestWorkflowReplayRequiresRegisteredCallInNodeAcceptanceCommit(t *testing.T) {
	var count atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &count)
	opts.Tools[0].Execution.RequestedGrantRef = "once"
	f := injectStore(t, &opts)
	w := newWorkflow(t, opts)
	submit(t, w)
	waitStopped(t, w)
	loaded, err := f.Load(t.Context(), opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
	// End immediately after node acceptance, where a missing call otherwise
	// has no later descriptor/observation to expose the inconsistency.
	var prefix []storage.Commit
	for _, c := range loaded.Commits {
		found := false
		var kept []storage.Record
		for _, r := range c.ControlRecords {
			if r.Type == "workflow_call" {
				found = true
				continue
			}
			kept = append(kept, r)
		}
		c.ControlRecords = kept
		prefix = append(prefix, c)
		if found {
			break
		}
	}
	loaded.Commits = prefix
	_, err = replay(loaded, opts.RunID)
	requireCode(t, err, product.CodeIncompatibleVersion)
}
func TestWorkflowReplayRejectsCompletedRunWithUnfinishedNode(t *testing.T) {
	var count atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &count)
	f := injectStore(t, &opts)
	w := newWorkflow(t, opts)
	submit(t, w)
	waitStopped(t, w)
	loaded, err := f.Load(context.Background(), opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range loaded.Commits {
		var kept []storage.Record
		for _, r := range loaded.Commits[i].ControlRecords {
			if r.Type == "workflow_node" {
				var p nodeRecord
				json.Unmarshal(r.Payload, &p)
				if p.Node.State == "completed" {
					continue
				}
			}
			kept = append(kept, r)
		}
		loaded.Commits[i].ControlRecords = kept
	}
	_, err = replay(loaded, opts.RunID)
	requireCode(t, err, product.CodeIncompatibleVersion)
}
