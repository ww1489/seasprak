package sessions

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/testkit"
)

// Seed the historical wire format, not an executable compatibility shim.
// This includes a real direct_resume/approval_binding association so the normal
// replay validators, rather than an invented paused trace alone, are exercised.
func legacyCommandFixture(t *testing.T, queued bool) (Options, *commandProcessProbe, string) {
	t.Helper()
	p := &commandProcessProbe{}
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Principal: "host", Profile: ProfileMemory,
		GenerationFingerprint: "legacy-command", Model: versionedPauseModel{testkit.NewFake()},
		Tools: []tools.Definition{builtinDefinitionForSession(t, "execute")}, Operations: tools.Operations{Process: p}}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	v := s.rt.manager.View()
	scope := agent.ExecutionScope{SessionID: opts.SessionID, BranchID: v.BranchID, TraceID: agent.MustID(), InvocationID: agent.MustID(), ExecutionID: agent.MustID(), Generation: s.rt.generation}
	target := agent.TargetAgent{Name: "direct-command", Version: "command-v1", Generation: scope.Generation}
	tr := state.TraceState{ID: scope.TraceID, Kind: "command", State: "running", Started: true, InvocationID: scope.InvocationID, ExecutionID: scope.ExecutionID, Target: target, Generation: scope.Generation, Limits: s.rt.opts.Limits}
	opID, inputID := agent.MustID(), agent.MustID()
	args := json.RawMessage(`{"argv":["echo","legacy"],"cwd":"workspace"}`)
	envelope, _ := json.Marshal(struct {
		OperationID string          `json:"operationId"`
		Name        string          `json:"name"`
		Arguments   json.RawMessage `json:"arguments"`
	}{opID, "execute", args})
	in := state.InputState{ID: inputID, TraceID: tr.ID, Kind: "command", State: "pending", Target: target, Content: envelope, CommitSeq: v.LastSeq + 1}
	op := state.Operation{Receipt: state.OperationReceipt{OperationID: opID, State: "accepted", Target: "execute", AcceptedCommit: v.LastSeq + 1}, Principal: "host", SessionID: opts.SessionID, Kind: "direct_command", Digest: "legacy-request-digest", Revision: 1, State: "accepted"}
	record := func(kind, id string, body any) store.Record {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return store.Record{Type: kind, Version: 1, ID: id, Payload: raw}
	}
	appendCommit := func(records []store.Record, entries []store.Record) {
		t.Helper()
		loaded, err := s.rt.opts.Store.Load(t.Context(), opts.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		seq := loaded.LastSeq
		_, err = s.rt.opts.Store.Append(t.Context(), opts.SessionID, store.ExpectedCommit{ExpectedPreviousSeq: seq}, store.Commit{RecordType: "commit", Version: 1, CommitID: agent.MustID(), CommitSeq: seq + 1, ExpectedPreviousSeq: seq, ControlRecords: records, Entries: entries})
		if err != nil {
			t.Fatal(err)
		}
	}
	controls := []store.Record{record("operation", opID, op)}
	op.State, op.Revision = "running", 2
	controls = append(controls, record("operation", opID, op))
	if queued {
		tr.State, tr.Started, tr.Hold = "queued", false, true
	}
	controls = append(controls, record("trace", tr.ID, tr), record("input", in.ID, in))
	if !queued {
		call := agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: opID, ProviderCallID: opID, OperationID: opID, Name: "execute", Arguments: string(args), Generation: scope.Generation}}
		frozen := agent.FrozenExecution{ID: "execution:" + opID, CallID: opID, Scope: scope, Origin: "direct", EntryPoint: "direct", Tool: "execute", Generation: scope.Generation, ProviderCallID: opID, OperationID: opID, RequestedGrantRef: "legacy-grant", PolicyRef: v.ExecutionPolicy.Ref}
		frozen.Hash, err = frozen.Digest()
		if err != nil {
			t.Fatal(err)
		}
		approvalID, interactionID := agent.MustID(), agent.MustID()
		approval := state.Approval{ID: approvalID, InteractionID: interactionID, CallID: opID, Scope: scope, FrozenExecutionID: frozen.ID, FrozenHash: frozen.Hash, GrantRef: frozen.RequestedGrantRef, State: "asked", ExpiresAt: time.Now().Add(time.Hour)}
		interaction := state.Interaction{ID: interactionID, Kind: "approval", Scope: scope, CallID: opID, ApprovalID: approvalID, State: "pending", ExpiresAt: approval.ExpiresAt}
		controls = append(controls, record("tool_call", opID, call), record("frozen_execution", frozen.ID, frozen), record("approval", approvalID, approval), record("interaction", interactionID, interaction))
		appendCommit(controls, nil)
		b := state.DirectResumeBinding{ID: agent.MustID(), Scope: scope, InputID: inputID, OperationID: opID, CallID: opID, ApprovalID: approvalID, InteractionID: interactionID, FrozenHash: frozen.Hash, BuildCompatibility: "legacy-build", EnvironmentFingerprint: "legacy-workspace", ManifestHash: "legacy-manifest", HistoryCommit: v.LastSeq + 1, LeafID: v.LeafID}
		binding := state.ApprovalBinding{ID: b.ID + ":" + interactionID, InteractionID: interactionID, ApprovalID: approvalID, DirectResumeID: b.ID}
		tr.State, tr.ExecutionStopped, tr.DirectResumeID = "paused", true, b.ID
		appendCommit([]store.Record{record("direct_resume", b.ID, b), record("approval_binding", binding.ID, binding), record("trace", tr.ID, tr)}, nil)
	} else {
		appendCommit(controls, nil)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	return opts, p, tr.ID
}

func TestP2CommandLegacyResumeRejected(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{true: "queued", false: "approval-wait"}[queued], func(t *testing.T) {
			opts, process, trace := legacyCommandFixture(t, queued)
			opened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			before := opened.rt.manager.View()
			snap, err := opened.Snapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if snap.Resume[trace].CanResume || snap.Resume[trace].Code != product.CodeIncompatibleResume {
				t.Fatal("legacy command advertised resumability")
			}
			_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: trace, ExpectedRevision: before.LastSeq})
			requireSessionCode(t, err, product.CodeIncompatibleResume)
			if queued {
				requireSessionCode(t, opened.ContinueQueue(t.Context(), trace), product.CodeIncompatibleResume)
			}
			if process.calls.Load() != 0 || !reflect.DeepEqual(before, opened.rt.manager.View()) {
				t.Fatal("legacy command read/resume executed or changed history")
			}
			if err := opened.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			opts.ReadOnly = true
			reader, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close(context.Background())
			if !reflect.DeepEqual(before, reader.rt.manager.View()) || process.calls.Load() != 0 {
				t.Fatal("read-only legacy history changed")
			}
		})
	}
}
