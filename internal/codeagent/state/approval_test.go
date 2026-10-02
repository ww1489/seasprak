package state_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// This interface keeps the first behavior tests compilable before the lifecycle
// exists. The ordinary ClaimTool regression below invokes an existing method.
type approvalManager interface {
	RequestApproval(context.Context, uint64, string, string, time.Time) (state.Interaction, error)
	CommitApprovalCheckpoint(context.Context, string, state.CheckpointRef, map[string]string) error
	RespondApproval(context.Context, state.OperationCommand, string, time.Time) (state.OperationReceipt, error)
	ClaimApprovedTool(context.Context, agent.FrozenCall, agent.Usage, string, string, time.Time) error
}

type approvalFixture struct {
	m      *state.Manager
	store  *failingStore
	call   agent.ToolRecord
	frozen agent.FrozenExecution
	now    time.Time
}

func approvalCall(t *testing.T) approvalFixture {
	t.Helper()
	m, backend := fixture(t)
	ctx := t.Context()
	if err := m.SetExecutionPolicy(ctx, 0, agent.ResolvedPolicy{}); err != nil {
		t.Fatal(err)
	}
	r := accept(t, m, "approval", `{"text":"hi"}`)
	if err := m.SetTraceState(ctx, r.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	scope := agent.ExecutionScope{SessionID: "session", BranchID: "main", TraceID: r.TraceID, InvocationID: m.View().Traces[r.TraceID].InvocationID, TurnID: "turn", ExecutionID: "original-execution", Generation: "g"}
	if err := m.SaveTurn(ctx, agent.TurnRecord{ID: "turn", TraceID: r.TraceID, InvocationID: scope.InvocationID}); err != nil {
		t.Fatal(err)
	}
	call := agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: "call", ProviderCallID: "provider", Name: "work", Arguments: `{}`, Generation: "g"}}
	msg := agent.AgentMessage{ID: "assistant", Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: "session", TraceID: r.TraceID, TurnID: "turn", InvocationID: scope.InvocationID}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider", Name: "work", Arguments: `{}`})}}}
	if err := m.SaveAssistant(ctx, msg, []agent.ToolRecord{call}); err != nil {
		t.Fatal(err)
	}
	frozen := agent.FrozenExecution{ID: "execution:call", CallID: "call", Scope: scope, Origin: "model", Tool: "work", Generation: "g", ProviderCallID: "provider", FinalArguments: json.RawMessage(`{}`), Effect: "read", BackendID: "trusted-run", PolicyRef: m.View().ExecutionPolicy.Ref, RequestedGrantRef: "one-operation"}
	var err error
	frozen.Hash, err = frozen.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SaveRecords(ctx, m.View().LastSeq, state.Records{FrozenExecutions: []state.FrozenExecution{frozen}}); err != nil {
		t.Fatal(err)
	}
	return approvalFixture{m: m, store: backend, call: call, frozen: frozen, now: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)}
}

func approvalLifecycle(t *testing.T, m *state.Manager) approvalManager {
	t.Helper()
	life, ok := any(m).(approvalManager)
	if !ok {
		t.Fatal("durable approval lifecycle is unavailable")
	}
	return life
}

func askApproval(t *testing.T, f approvalFixture) state.Interaction {
	t.Helper()
	in, err := approvalLifecycle(t, f.m).RequestApproval(t.Context(), f.m.View().LastSeq, f.frozen.ID, "Approve this operation once?", f.now)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func bindApproval(t *testing.T, f approvalFixture, in state.Interaction) state.CheckpointRef {
	t.Helper()
	v := f.m.View()
	cp := state.CheckpointRef{ID: "approval-checkpoint", BlobHash: "saved-blob", BlobSize: 100, Scope: f.call.Scope, Target: v.Traces[f.call.Scope.TraceID].Target, HistoryCommit: v.LastSeq, ProjectionRevision: v.LastSeq, LeafID: v.LeafID, UnfinishedTurnIDs: []string{"turn"}, CallIDs: []string{f.call.Call.CallID}, InteractionIDs: []string{in.ID}}
	if err := approvalLifecycle(t, f.m).CommitApprovalCheckpoint(t.Context(), f.call.Scope.TraceID, cp, map[string]string{in.ID: "server-interrupt-target"}); err != nil {
		t.Fatal(err)
	}
	return cp
}

func approvalResponse(m *state.Manager, in state.Interaction, decision string) state.OperationCommand {
	content, _ := json.Marshal(struct {
		Decision string `json:"decision"`
	}{decision})
	return state.OperationCommand{Principal: "host-user", Kind: "respond_interaction", Target: in.ID, IdempotencyKey: "answer", ExpectedRevision: m.View().LastSeq, Content: content}
}

func approveAndResume(t *testing.T, f approvalFixture) (state.Interaction, state.OperationCommand, state.OperationReceipt) {
	t.Helper()
	in := askApproval(t, f)
	cp := bindApproval(t, f, in)
	cmd := approvalResponse(f.m, in, "allowed-once")
	receipt, err := approvalLifecycle(t, f.m).RespondApproval(t.Context(), cmd, "allowed-once", f.now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.CommitResume(t.Context(), state.OperationCommand{Kind: "resume", Target: f.call.Scope.TraceID, ExpectedRevision: f.m.View().LastSeq}, cp.ID, "resumed-execution"); err != nil {
		t.Fatal(err)
	}
	return in, cmd, receipt
}

func TestApprovalRequiredCallCannotUseOrdinaryClaim(t *testing.T) {
	f := approvalCall(t)
	before := f.m.View()
	usage := before.Traces[f.call.Scope.TraceID].Usage
	usage.ToolExecutions++
	err := f.m.ClaimTool(t.Context(), f.call.Call, usage)
	requireP2Code(t, err, product.CodePermissionDenied)
	if !reflect.DeepEqual(before, f.m.View()) {
		t.Fatal("missing approval consumed call or budget")
	}
}

func TestApprovalAskedIsDurableStableAndCannotAnswerBeforeCheckpoint(t *testing.T) {
	f := approvalCall(t)
	in := askApproval(t, f)
	v := f.m.View()
	approval := v.Approvals[in.ApprovalID]
	if in.ID == "" || in.ApprovalID == "" || in.CallID != f.call.Call.CallID || in.Scope != f.call.Scope || in.CheckpointRef != "" || in.TargetRef != "" || in.State != "pending" || approval.State != "asked" || approval.FrozenHash != f.frozen.Hash || !in.ExpiresAt.Equal(f.now.Add(24*time.Hour)) || !approval.ExpiresAt.Equal(in.ExpiresAt) {
		t.Fatalf("interaction=%+v approval=%+v", in, approval)
	}
	reopened, err := state.NewManager(f.store, "session")
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := approvalLifecycle(t, reopened).RequestApproval(t.Context(), 0, f.frozen.ID, in.Question, f.now.Add(time.Hour))
	if err != nil || !reflect.DeepEqual(in, repeated) || !reflect.DeepEqual(v, reopened.View()) {
		t.Fatalf("repeated ask drifted: %+v %v", repeated, err)
	}
	_, err = approvalLifecycle(t, f.m).RespondApproval(t.Context(), approvalResponse(f.m, in, "allowed-once"), "allowed-once", f.now)
	requireP2Code(t, err, product.CodeStateConflict)
	if !reflect.DeepEqual(v, f.m.View()) {
		t.Fatal("unassociated approval accepted a decision")
	}
}

func TestApprovalDecisionDoesNotExecuteAndRetryKeepsOriginalReceipt(t *testing.T) {
	for _, decision := range []string{"allowed-once", "rejected", "cancelled"} {
		t.Run(decision, func(t *testing.T) {
			f := approvalCall(t)
			in := askApproval(t, f)
			bindApproval(t, f, in)
			before := f.m.View()
			cmd := approvalResponse(f.m, in, decision)
			receipt, err := approvalLifecycle(t, f.m).RespondApproval(t.Context(), cmd, decision, f.now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			v := f.m.View()
			if v.LastSeq != before.LastSeq+1 || v.Calls[f.call.Call.CallID].Claimed || v.Calls[f.call.Call.CallID].Observation != nil || v.Traces[f.call.Scope.TraceID].State != "paused" || v.Traces[f.call.Scope.TraceID].Usage != before.Traces[f.call.Scope.TraceID].Usage || len(v.Messages) != len(before.Messages) || v.Turns["turn"].Ended {
				t.Fatal("answer changed execution, budget, history, or unfinished turn")
			}
			reopened, err := state.NewManager(f.store, "session")
			if err != nil {
				t.Fatal(err)
			}
			retried, err := approvalLifecycle(t, reopened).RespondApproval(t.Context(), cmd, decision, f.now.Add(25*time.Hour))
			if err != nil || retried != receipt || !reflect.DeepEqual(v, reopened.View()) {
				t.Fatalf("lost original answer receipt: %+v %v", retried, err)
			}
			conflict := cmd
			conflict.Content = json.RawMessage(`{"decision":"different"}`)
			_, err = approvalLifecycle(t, reopened).RespondApproval(t.Context(), conflict, decision, f.now)
			requireP2Code(t, err, product.CodeIdempotencyConflict)
		})
	}
}

func TestApprovalClaimIsAtomicAndHasOneConcurrentWinner(t *testing.T) {
	f := approvalCall(t)
	in, _, _ := approveAndResume(t, f)
	before := f.m.View()
	usage := before.Traces[f.call.Scope.TraceID].Usage
	usage.ToolExecutions++
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if approvalLifecycle(t, f.m).ClaimApprovedTool(t.Context(), f.call.Call, usage, in.ApprovalID, "resumed-execution", f.now.Add(2*time.Hour)) == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	v := f.m.View()
	if winners.Load() != 1 || v.LastSeq != before.LastSeq+1 || !v.Calls[f.call.Call.CallID].Claimed || v.Traces[f.call.Scope.TraceID].Usage.ToolExecutions != 1 {
		t.Fatalf("winners=%d call=%+v usage=%+v", winners.Load(), v.Calls[f.call.Call.CallID], v.Traces[f.call.Scope.TraceID].Usage)
	}
	reopened, err := state.NewManager(f.store, "session")
	if err != nil || !reflect.DeepEqual(v, reopened.View()) {
		t.Fatalf("claim not recovered atomically: %v", err)
	}
}

func TestApprovalExpiresBeforeDecisionAndAgainBeforeClaim(t *testing.T) {
	t.Run("decision", func(t *testing.T) {
		f := approvalCall(t)
		in := askApproval(t, f)
		bindApproval(t, f, in)
		before := f.m.View()
		_, err := approvalLifecycle(t, f.m).RespondApproval(t.Context(), approvalResponse(f.m, in, "allowed-once"), "allowed-once", in.ExpiresAt)
		requireP2Code(t, err, product.CodePermissionDenied)
		if !reflect.DeepEqual(before, f.m.View()) {
			t.Fatal("expired decision wrote state")
		}
	})
	t.Run("claim", func(t *testing.T) {
		f := approvalCall(t)
		in, _, _ := approveAndResume(t, f)
		before := f.m.View()
		usage := before.Traces[f.call.Scope.TraceID].Usage
		usage.ToolExecutions++
		err := approvalLifecycle(t, f.m).ClaimApprovedTool(t.Context(), f.call.Call, usage, in.ApprovalID, "resumed-execution", in.ExpiresAt)
		requireP2Code(t, err, product.CodePermissionDenied)
		if !reflect.DeepEqual(before, f.m.View()) {
			t.Fatal("expired claim consumed allowance")
		}
	})
}

func TestApprovalClaimCommitFailureLeaksNeitherGrantNorBudget(t *testing.T) {
	f := approvalCall(t)
	in, _, _ := approveAndResume(t, f)
	before := f.m.View()
	usage := before.Traces[f.call.Scope.TraceID].Usage
	usage.ToolExecutions++
	f.store.fail = true
	if err := approvalLifecycle(t, f.m).ClaimApprovedTool(t.Context(), f.call.Call, usage, in.ApprovalID, "resumed-execution", f.now.Add(time.Hour)); err == nil {
		t.Fatal("claim should fail")
	}
	if !reflect.DeepEqual(before, f.m.View()) {
		t.Fatal("failed commit exposed claim or budget")
	}
}

func TestApprovalRejectedAndCancelledDecisionsCannotBeClaimed(t *testing.T) {
	for _, decision := range []string{"rejected", "cancelled"} {
		t.Run(decision, func(t *testing.T) {
			f := approvalCall(t)
			life := approvalLifecycle(t, f.m)
			in := askApproval(t, f)
			cp := bindApproval(t, f, in)
			if _, err := life.RespondApproval(t.Context(), approvalResponse(f.m, in, decision), decision, f.now); err != nil {
				t.Fatal(err)
			}
			if _, err := f.m.CommitResume(t.Context(), state.OperationCommand{Kind: "resume", Target: f.call.Scope.TraceID, ExpectedRevision: f.m.View().LastSeq}, cp.ID, "resumed-execution"); err != nil {
				t.Fatal(err)
			}
			before := f.m.View()
			usage := before.Traces[f.call.Scope.TraceID].Usage
			usage.ToolExecutions++
			err := life.ClaimApprovedTool(t.Context(), f.call.Call, usage, in.ApprovalID, "resumed-execution", f.now)
			requireP2Code(t, err, product.CodePermissionDenied)
			if !reflect.DeepEqual(before, f.m.View()) {
				t.Fatal("non-approval consumed allowance")
			}
		})
	}
}

func TestApprovalRevalidatesCurrentPolicyAtDecisionAndClaim(t *testing.T) {
	for _, boundary := range []string{"decision", "claim"} {
		t.Run(boundary, func(t *testing.T) {
			f := approvalCall(t)
			life := approvalLifecycle(t, f.m)
			var in state.Interaction
			if boundary == "claim" {
				in, _, _ = approveAndResume(t, f)
			} else {
				in = askApproval(t, f)
				bindApproval(t, f, in)
			}
			if err := f.m.SetExecutionPolicy(t.Context(), f.m.View().ExecutionPolicy.Revision, agent.ResolvedPolicy{ApprovalPolicy: "never"}); err != nil {
				t.Fatal(err)
			}
			before := f.m.View()
			var err error
			if boundary == "claim" {
				usage := before.Traces[f.call.Scope.TraceID].Usage
				usage.ToolExecutions++
				err = life.ClaimApprovedTool(t.Context(), f.call.Call, usage, in.ApprovalID, "resumed-execution", f.now.Add(2*time.Hour))
			} else {
				_, err = life.RespondApproval(t.Context(), approvalResponse(f.m, in, "allowed-once"), "allowed-once", f.now)
			}
			requireP2Code(t, err, product.CodePermissionDenied)
			if !reflect.DeepEqual(before, f.m.View()) {
				t.Fatal("policy rejection appended approval state")
			}
		})
	}
}

func TestApprovalClaimRejectsWrongApprovalOrExecution(t *testing.T) {
	for _, wrong := range []string{"approval", "execution"} {
		t.Run(wrong, func(t *testing.T) {
			f := approvalCall(t)
			in, _, _ := approveAndResume(t, f)
			approvalID, executionID := in.ApprovalID, "resumed-execution"
			if wrong == "approval" {
				approvalID = "not-this-approval"
			} else {
				executionID = f.call.Scope.ExecutionID
			}
			before := f.m.View()
			usage := before.Traces[f.call.Scope.TraceID].Usage
			usage.ToolExecutions++
			err := approvalLifecycle(t, f.m).ClaimApprovedTool(t.Context(), f.call.Call, usage, approvalID, executionID, f.now.Add(2*time.Hour))
			requireP2Code(t, err, product.CodePermissionDenied)
			if !reflect.DeepEqual(before, f.m.View()) {
				t.Fatal("wrong source claimed the original call")
			}
		})
	}
}

func TestApprovalAskCheckpointAndAnswerFailuresLeaveViewUnchanged(t *testing.T) {
	for _, boundary := range []string{"ask", "checkpoint", "answer"} {
		t.Run(boundary, func(t *testing.T) {
			f := approvalCall(t)
			life := approvalLifecycle(t, f.m)
			var in state.Interaction
			if boundary != "ask" {
				in = askApproval(t, f)
			}
			if boundary == "answer" {
				bindApproval(t, f, in)
			}
			before := f.m.View()
			f.store.fail = true
			var err error
			switch boundary {
			case "ask":
				_, err = life.RequestApproval(t.Context(), before.LastSeq, f.frozen.ID, "Approve?", f.now)
			case "checkpoint":
				cp := state.CheckpointRef{ID: "checkpoint", BlobHash: "saved", BlobSize: 100, Scope: f.call.Scope, Target: before.Traces[f.call.Scope.TraceID].Target, HistoryCommit: before.LastSeq, ProjectionRevision: before.LastSeq, LeafID: before.LeafID, CallIDs: []string{f.call.Call.CallID}, UnfinishedTurnIDs: []string{f.call.Scope.TurnID}, InteractionIDs: []string{in.ID}}
				err = life.CommitApprovalCheckpoint(t.Context(), f.call.Scope.TraceID, cp, map[string]string{in.ID: "target"})
			case "answer":
				_, err = life.RespondApproval(t.Context(), approvalResponse(f.m, in, "allowed-once"), "allowed-once", f.now)
			}
			if err == nil || !reflect.DeepEqual(before, f.m.View()) {
				t.Fatalf("failed %s changed view: %v", boundary, err)
			}
		})
	}
}

func TestApprovalResponseRejectsParametersAndMissingPrincipal(t *testing.T) {
	for _, bad := range []string{"parameters", "decision", "principal", "revision"} {
		t.Run(bad, func(t *testing.T) {
			f := approvalCall(t)
			in := askApproval(t, f)
			bindApproval(t, f, in)
			before := f.m.View()
			cmd := approvalResponse(f.m, in, "allowed-once")
			code := product.CodeInvalidArgument
			switch bad {
			case "parameters":
				cmd.Content = json.RawMessage(`{"decision":"allowed-once","arguments":{}}`)
			case "decision":
				cmd.Content = json.RawMessage(`{"decision":"rejected"}`)
			case "principal":
				cmd.Principal = ""
				code = product.CodePermissionDenied
			case "revision":
				cmd.ExpectedRevision--
				code = product.CodeStateConflict
			}
			_, err := approvalLifecycle(t, f.m).RespondApproval(t.Context(), cmd, "allowed-once", f.now)
			requireP2Code(t, err, code)
			if !reflect.DeepEqual(before, f.m.View()) {
				t.Fatal("invalid answer wrote an operation or decision")
			}
		})
	}
}

func TestApprovalUnansweredCallKeepsBindingAcrossRepeatedResumes(t *testing.T) {
	f := approvalCall(t)
	life := approvalLifecycle(t, f.m)
	in := askApproval(t, f)
	cp := bindApproval(t, f, in)
	original := f.m.View().Approvals[in.ApprovalID]
	for index, executionID := range []string{"resume-one", "resume-two", "resume-three"} {
		if _, err := f.m.CommitResume(t.Context(), state.OperationCommand{Kind: "resume", Target: f.call.Scope.TraceID, ExpectedRevision: f.m.View().LastSeq}, cp.ID, executionID); err != nil {
			t.Fatal(err)
		}
		v := f.m.View()
		cp.ID = executionID + "-checkpoint"
		cp.Scope.ExecutionID = executionID
		cp.HistoryCommit, cp.ProjectionRevision, cp.LeafID = v.LastSeq, v.LastSeq, v.LeafID
		if err := life.CommitApprovalCheckpoint(t.Context(), f.call.Scope.TraceID, cp, map[string]string{in.ID: executionID + "-target"}); err != nil {
			t.Fatalf("waiting checkpoint %d lost original call: %v", index+1, err)
		}
		v = f.m.View()
		if !reflect.DeepEqual(original, v.Approvals[in.ApprovalID]) || v.Calls[f.call.Call.CallID].Claimed || v.Traces[f.call.Scope.TraceID].Usage.ToolExecutions != 0 {
			t.Fatal("repeated waiting changed approval or budget")
		}
	}
}

func TestApprovalReplayRejectsOperationFromAnotherSession(t *testing.T) {
	f := approvalCall(t)
	approveAndResume(t, f)
	_, err := state.NewManager(approvalMutationStore{Store: f.store, kind: "operation", mutate: func(r *store.Record) {
		var op state.Operation
		if err := json.Unmarshal(r.Payload, &op); err != nil {
			t.Fatal(err)
		}
		op.SessionID = "another-session"
		r.Payload, _ = json.Marshal(op)
	}}, "session")
	requireP2Code(t, err, product.CodeIncompatibleVersion)
}

type approvalPartialCommitStore struct{ store.Store }

func (s approvalPartialCommitStore) Load(ctx context.Context, id string) (store.StoredSession, error) {
	saved, err := s.Store.Load(ctx, id)
	if err != nil {
		return saved, err
	}
	for i := range saved.Commits {
		hasClaim := false
		for _, r := range saved.Commits[i].ControlRecords {
			hasClaim = hasClaim || r.Type == "approval_claim"
		}
		if hasClaim {
			var records []store.Record
			for _, r := range saved.Commits[i].ControlRecords {
				if r.Type != "trace" {
					records = append(records, r)
				}
			}
			saved.Commits[i].ControlRecords = records
			return saved, nil
		}
	}
	panic("claim commit missing")
}

func TestApprovalReplayRejectsClaimWithoutAtomicBudget(t *testing.T) {
	f := approvalCall(t)
	in, _, _ := approveAndResume(t, f)
	usage := f.m.View().Traces[f.call.Scope.TraceID].Usage
	usage.ToolExecutions++
	if err := approvalLifecycle(t, f.m).ClaimApprovedTool(t.Context(), f.call.Call, usage, in.ApprovalID, "resumed-execution", f.now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err := state.NewManager(approvalPartialCommitStore{f.store}, "session")
	requireP2Code(t, err, product.CodeIncompatibleVersion)
}

func TestApprovalBudgetExhaustionDoesNotConsumeTheGrant(t *testing.T) {
	f := approvalCall(t)
	in, _, _ := approveAndResume(t, f)
	usage := f.m.View().Traces[f.call.Scope.TraceID].Usage
	usage.ToolExecutions = f.m.View().Traces[f.call.Scope.TraceID].Limits.TraceToolCalls
	if err := f.m.SaveTraceBudget(t.Context(), f.call.Scope.TraceID, usage); err != nil {
		t.Fatal(err)
	}
	before := f.m.View()
	usage.ToolExecutions++
	err := approvalLifecycle(t, f.m).ClaimApprovedTool(t.Context(), f.call.Call, usage, in.ApprovalID, "resumed-execution", f.now.Add(2*time.Hour))
	requireP2Code(t, err, product.CodeBudgetExhausted)
	if !reflect.DeepEqual(before, f.m.View()) || len(f.m.View().ApprovalClaims) != 0 {
		t.Fatal("budget rejection consumed the approval")
	}
}

func TestApprovalClaimRecordsGrantOriginalIntentAndBudgetInOneCommit(t *testing.T) {
	f := approvalCall(t)
	in, _, _ := approveAndResume(t, f)
	originalApproval := f.m.View().Approvals[in.ApprovalID]
	originalInteraction := f.m.View().Interactions[in.ID]
	usage := f.m.View().Traces[f.call.Scope.TraceID].Usage
	usage.ToolExecutions++
	if err := approvalLifecycle(t, f.m).ClaimApprovedTool(t.Context(), f.call.Call, usage, in.ApprovalID, "resumed-execution", f.now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	saved, err := f.store.Load(t.Context(), "session")
	if err != nil {
		t.Fatal(err)
	}
	last := saved.Commits[len(saved.Commits)-1]
	if len(last.ControlRecords) != 3 || last.ControlRecords[0].Type != "trace" || last.ControlRecords[1].Type != "tool_call" || last.ControlRecords[2].Type != "approval_claim" {
		t.Fatalf("claim facts are not one commit: %+v", last.ControlRecords)
	}
	v := f.m.View()
	if v.ApprovalClaims[in.ApprovalID].CallID != f.call.Call.CallID || v.ApprovalClaims[in.ApprovalID].ExecutionID != "resumed-execution" || !reflect.DeepEqual(originalApproval, v.Approvals[in.ApprovalID]) || !reflect.DeepEqual(originalInteraction, v.Interactions[in.ID]) {
		t.Fatal("claim changed the original requested approval")
	}
	reopened, err := state.NewManager(f.store, "session")
	if err != nil {
		t.Fatal(err)
	}
	before := reopened.View()
	err = approvalLifecycle(t, reopened).ClaimApprovedTool(t.Context(), f.call.Call, usage, in.ApprovalID, "resumed-execution", f.now.Add(2*time.Hour))
	requireP2Code(t, err, product.CodeStateConflict)
	if !reflect.DeepEqual(before, reopened.View()) {
		t.Fatal("reopened claim consumed approval a second time")
	}
}

type approvalMutationStore struct {
	store.Store
	kind   string
	mutate func(*store.Record)
}

func (s approvalMutationStore) Load(ctx context.Context, id string) (store.StoredSession, error) {
	saved, err := s.Store.Load(ctx, id)
	if err != nil {
		return saved, err
	}
	for i := range saved.Commits {
		for j := range saved.Commits[i].ControlRecords {
			r := &saved.Commits[i].ControlRecords[j]
			if r.Type == s.kind {
				s.mutate(r)
				return saved, nil
			}
		}
	}
	panic("approval record missing from replay fixture")
}

func TestApprovalReplayRejectsUnknownVersionsAndWrongReferences(t *testing.T) {
	for _, kind := range []string{"approval_binding", "approval_decision", "approval_claim"} {
		for _, change := range []string{"version", "identity"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				f := approvalCall(t)
				in, _, _ := approveAndResume(t, f)
				usage := f.m.View().Traces[f.call.Scope.TraceID].Usage
				usage.ToolExecutions++
				if err := approvalLifecycle(t, f.m).ClaimApprovedTool(t.Context(), f.call.Call, usage, in.ApprovalID, "resumed-execution", f.now.Add(2*time.Hour)); err != nil {
					t.Fatal(err)
				}
				_, err := state.NewManager(approvalMutationStore{Store: f.store, kind: kind, mutate: func(r *store.Record) {
					if change == "version" {
						r.Version++
						return
					}
					var data map[string]json.RawMessage
					if err := json.Unmarshal(r.Payload, &data); err != nil {
						t.Fatal(err)
					}
					field := map[string]string{"approval_binding": "interactionId", "approval_decision": "principal", "approval_claim": "executionId"}[kind]
					data[field] = json.RawMessage(`"unrelated"`)
					r.Payload, _ = json.Marshal(data)
				}}, "session")
				requireP2Code(t, err, product.CodeIncompatibleVersion)
			})
		}
	}
}
