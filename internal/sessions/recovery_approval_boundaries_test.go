package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// Detect actual product records, never timing or callback names. The stopped
// record is emitted by ConfirmExecutionStopped after the resumed segment exits.
func recoveryApprovalCommitKind(c store.Commit) string {
	for _, ev := range c.Events {
		if ev.Type == "trace.settled" {
			return "settled"
		}
	}
	for _, r := range c.ControlRecords {
		switch r.Type {
		case "checkpoint_ref":
			return "checkpoint"
		case "operation":
			var op state.Operation
			if json.Unmarshal(r.Payload, &op) == nil && op.Kind == "resume" {
				if op.State == "accepted" {
					return "resume"
				}
				if op.State == "completed" {
					return "segment"
				}
			}
		case "tool_call":
			var call agent.ToolRecord
			if json.Unmarshal(r.Payload, &call) == nil {
				if call.Observation != nil {
					return "observation"
				}
				if call.Claimed {
					return "claim"
				}
			}
		case "trace":
			var tr state.TraceState
			if json.Unmarshal(r.Payload, &tr) == nil && tr.State == "running" && tr.ExecutionStopped {
				return "stopped"
			}
		}
	}
	return ""
}

func assertRecoveryApprovalExtended(t *testing.T, root, window string, rep recoveryApprovalReport) {
	t.Helper()
	opts, model, process := recoveryApprovalOptions(t, root)
	stored, err := loadCrashJournal(opts.StateRoot, opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Target.CommitID != "" {
		expected := append([]store.Commit(nil), rep.Prefix...)
		if strings.HasSuffix(window, "_after") {
			expected = append(expected, rep.Target)
		}
		if !reflect.DeepEqual(expected, stored.Commits) {
			t.Fatal("kill did not preserve the exact committed prefix")
		}
	}
	tr := rep.Durable.Traces[rep.TraceID]
	if tr == nil || len(rep.Durable.Calls) != 1 {
		t.Fatal("missing original trace/call")
	}
	var call agent.ToolRecord
	for _, c := range rep.Durable.Calls {
		call = c
	}
	late := strings.HasPrefix(window, "segment_") || strings.HasPrefix(window, "stopped_") || strings.HasPrefix(window, "settled_")
	checkpoint := strings.HasPrefix(window, "checkpoint_")
	claimed := !checkpoint && window != "claim_before"
	observed := window == "observation_after" || late
	effects := 0
	if strings.HasPrefix(window, "execute_") || strings.HasPrefix(window, "observation_") || late {
		effects = 1
	}
	returned := effects
	if window == "execute_entered" {
		returned = 0
	}
	if rep.ReturnedCalls != returned {
		t.Fatalf("backend successful returns=%d/%d", rep.ReturnedCalls, returned)
	}
	models := 1
	if late {
		models = 2
	}
	if rep.ModelCalls != models || rep.ToolCalls != effects {
		t.Fatalf("barrier calls model=%d/%d backend=%d/%d", rep.ModelCalls, models, rep.ToolCalls, effects)
	}
	toolBudget := 0
	if claimed {
		toolBudget = 1
	}
	if call.Claimed != claimed || (call.Observation != nil) != observed || tr.Usage.ToolExecutions != toolBudget || tr.Usage.LogicalModelCalls != models {
		t.Fatal("claim/observation/logical budget does not match the crash boundary")
	}
	if observed && (call.Observation.Status != "succeeded" || !call.Observation.Executed) {
		t.Fatalf("actual backend result not retained: status=%s executed=%v content=%s", call.Observation.Status, call.Observation.Executed, call.Observation.Content)
	}
	if !checkpoint {
		original := rep.Before.Calls[call.Call.CallID]
		if call.Call != original.Call || call.Scope != original.Scope || !reflect.DeepEqual(rep.Before.FrozenExecutions, rep.Durable.FrozenExecutions) {
			t.Fatal("resume changed original call identity or frozen execution")
		}
		if len(rep.Durable.ResumedExecutions) != 1 || len(rep.Durable.Operations) != 1 {
			t.Fatal("resume acceptance or execution segment lost")
		}
		for _, segment := range rep.Durable.ResumedExecutions {
			if segment.Scope.ExecutionID == call.Scope.ExecutionID || !sameLogicalScope(segment.Scope, call.Scope) || segment.CheckpointID == "" {
				t.Fatal("resume segment lost original logical scope")
			}
		}
		completed := window == "segment_after" || strings.HasPrefix(window, "stopped_") || strings.HasPrefix(window, "settled_")
		for _, op := range rep.Durable.Operations {
			if (op.State == "completed") != completed || op.Receipt.State != "accepted" {
				t.Fatal("resume operation completion differs from durable segment exit")
			}
		}
	}
	settled := window == "settled_after"
	wantSettled := 0
	if settled {
		wantSettled = 1
	}
	if countSettled(stored) != wantSettled || tr.Settled != settled {
		t.Fatal("terminal event is missing, duplicated, or premature")
	}
	if late {
		stopped := window == "stopped_after" || strings.HasPrefix(window, "settled_")
		if tr.ExecutionStopped != stopped {
			t.Fatal("execution exit proof disagrees with the commit window")
		}
		if settled && tr.State != "completed" {
			t.Fatal("settled trace was not completed")
		}
	}
	if checkpoint {
		wantRefs := 0
		if window == "checkpoint_after" {
			wantRefs = 1
		}
		if len(rep.Durable.Checkpoints) != wantRefs {
			t.Fatal("checkpoint association crossed the barrier")
		}
	}
	// Read-only first proves crash inspection changes neither bytes nor facts.
	// Writable Open may append the documented running -> paused transition only.
	for _, readOnly := range []bool{true, false} {
		opts.ReadOnly = readOnly
		beforeBytes, err := os.ReadFile(journalPath(opts.StateRoot, opts.SessionID))
		if err != nil {
			t.Fatal(err)
		}
		opened, err := OpenAgentSession(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = opened.Close(context.Background()) })
		snap := approvalSnapshot(t, opened)
		if len(snap.Interactions) != 0 || len(snap.Approvals) != 0 {
			t.Fatal("Open re-created runtime approval")
		}
		v := opened.rt.manager.View()
		if !reflect.DeepEqual(rep.Durable.Calls, v.Calls) || !reflect.DeepEqual(rep.Durable.Observations, v.Observations) || !reflect.DeepEqual(rep.Durable.Traces[rep.TraceID].Usage, v.Traces[rep.TraceID].Usage) || !reflect.DeepEqual(rep.Durable.Checkpoints, v.Checkpoints) || !reflect.DeepEqual(rep.Durable.ResumedExecutions, v.ResumedExecutions) || !reflect.DeepEqual(rep.Durable.Operations, v.Operations) {
			t.Fatal("reopen changed persisted call, budget, checkpoint or resume evidence")
		}
		if checkpoint {
			// The blob is fully durable on BOTH sides of checkpoint_ref association.
			// Before association it is an orphan, not permission to execute.
			var cp state.CheckpointRef
			for _, r := range rep.Target.ControlRecords {
				if r.Type == "checkpoint_ref" {
					if err := json.Unmarshal(r.Payload, &cp); err != nil {
						t.Fatal(err)
					}
				}
			}
			blobs, ok := opened.rt.opts.Store.(store.CheckpointBlobs)
			if !ok || cp.BlobHash == "" || cp.BlobSize <= 0 {
				t.Fatal("missing checkpoint blob capability/ref")
			}
			ref := store.BlobRef{Hash: cp.BlobHash, Size: cp.BlobSize}
			data, err := blobs.Get(t.Context(), opts.SessionID, ref)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.VerifyBlob(ref, data); err != nil {
				t.Fatal(err)
			}
		}
		if !readOnly && rep.Response.InteractionID != "" {
			rep.Response.ExpectedRevision = v.LastSeq
			_, err := opened.RespondInteraction(t.Context(), rep.Response)
			requireSessionCode(t, err, product.CodeNotFound)
			if !reflect.DeepEqual(v, opened.rt.manager.View()) {
				t.Fatal("stale approval modified durable state")
			}
		}
		if model.Calls() != 0 || process.calls.Load() != 0 {
			t.Fatal("reopen started model/backend")
		}
		if n, err := readEffect(filepath.Join(root, "effect")); err != nil || n != effects {
			t.Fatalf("backend effects changed across reopen: %d err=%v", n, err)
		}
		if n, err := readEffect(filepath.Join(root, "effect-returned")); err != nil || n != returned {
			t.Fatalf("backend returns changed across reopen: %d err=%v", n, err)
		}
		if err := opened.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		afterBytes, err := os.ReadFile(journalPath(opts.StateRoot, opts.SessionID))
		if err != nil {
			t.Fatal(err)
		}
		after, err := loadCrashJournal(opts.StateRoot, opts.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if readOnly || tr.State != "running" {
			if !bytes.Equal(beforeBytes, afterBytes) {
				t.Fatal("browsing/stopped reopen wrote journal")
			}
		} else if len(after.Commits) != len(stored.Commits)+1 || v.Traces[rep.TraceID].State != "paused" {
			t.Fatal("writable recovery was not exactly one paused transition")
		}
		if countSettled(after) != wantSettled {
			t.Fatal("reopen invented a terminal event")
		}
	}
}
