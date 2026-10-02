package state_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	store "github.com/ww1489/seasprak/internal/storage"
)

func childAttemptDetails(initial state.ModelAttempt) agent.ModelAttemptDetails {
	return agent.ModelAttemptDetails{Usage: []agent.ModelRequestUsage{{Request: llm.TransportRequest{ModelCallID: initial.ModelCallID, AttemptID: initial.ID, TransportAttempt: 1, Purpose: "agent"}, Snapshot: llm.UsageSnapshot{Usage: llm.UsageRecord{InputTotal: llm.UsageValue{Known: true, Value: 12, Source: "fixture"}}}}}}
}

func addSecondChildTool(msg *agent.AgentMessage, calls *[]agent.ToolRecord) {
	second := (*calls)[0]
	second.Call.CallID, second.Call.ProviderCallID, second.Call.Name, second.Call.Arguments = "second-child-call", "second-child-provider", "other", `{"second":true}`
	*calls = append(*calls, second)
	msg.Standard.ContentBlocks = append(msg.Standard.ContentBlocks, schema.NewContentBlock(&schema.FunctionToolCall{CallID: second.Call.ProviderCallID, Name: second.Call.Name, Arguments: second.Call.Arguments}))
}

func requireChildSafeEvents(t *testing.T, events []agent.Event, initial state.ModelAttempt, status string) {
	t.Helper()
	if len(events) != 1 || events[0].Type != "model.attempt_finalized" || events[0].Scope.SessionID != initial.Scope.SessionID || events[0].Scope.TraceID != initial.Scope.TraceID || events[0].Scope.TurnID != "" {
		t.Fatal("child terminal must publish one safe identity event without a parent Turn")
	}
	var payload map[string]string
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"attemptId": initial.ID, "messageId": initial.MessageID, "invocationId": initial.Scope.InvocationID, "state": status}
	if !reflect.DeepEqual(payload, want) || strings.Contains(string(events[0].Payload), "child-private-evidence") {
		t.Fatal("child terminal event exposed non-allowlisted material")
	}
}

func TestP3ChildAttemptAcceptanceOwnsPrivateMessagesCallsAndEvidence(t *testing.T) {
	m, backend, initial, msg, calls := childAttemptFixture(t)
	addSecondChildTool(&msg, &calls)
	msg.Standard.ContentBlocks[0].Extra = map[string]any{"block-private": "child-private-block"}
	msg.Standard.ContentBlocks = append(msg.Standard.ContentBlocks,
		schema.NewContentBlock(&schema.Reasoning{Text: "child-private-reasoning", Signature: "synthetic-child-private-signature"}),
		schema.NewContentBlock(&schema.AssistantGenText{Text: "child-private-answer", Extension: map[string]any{"text-private": []any{true, nil, float64(1.25)}}}))
	msg.Standard.ResponseMeta = &schema.AgenticResponseMeta{Extension: map[string]any{"response-private": "child-private-response"}}
	details := childAttemptDetails(initial)
	before := m.View()
	if err := m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "accepted", "tool_calls", &msg, calls, details); err != nil {
		t.Fatal(err)
	}
	after := m.View()
	requireChildParentUnchanged(t, before, after)
	requireChildSafeEvents(t, after.Events[len(before.Events):], initial, "accepted")
	inv := after.Invocations[initial.Scope.InvocationID]
	if !reflect.DeepEqual(inv.MessageIDs, []string{initial.MessageID}) || !reflect.DeepEqual(inv.CallIDs, []string{calls[0].Call.CallID, calls[1].Call.CallID}) || inv.ModelCalls != before.Invocations[inv.ID].ModelCalls {
		t.Fatal("child acceptance changed model accounting or lost accepted order")
	}
	if !reflect.DeepEqual(after.InvocationMessages[msg.ID], msg) || after.ModelAttempts[initial.ID] != initial || after.AttemptResults[initial.ID].Revision != 2 || len(after.UnfinishedAttempts) != 0 {
		t.Fatal("child acceptance changed initial identity or lost the full private candidate")
	}
	for _, call := range calls {
		if !reflect.DeepEqual(after.Calls[call.Call.CallID], call) {
			t.Fatal("child tool provider or frozen identity changed")
		}
	}
	terminal := after.AttemptResults[initial.ID]
	if terminal.UsageRef != terminal.DiagnosticRef || !reflect.DeepEqual(after.AttemptDetails[terminal.UsageRef].ModelAttemptDetails, details) || after.Budget != before.Budget || after.Traces[initial.Scope.TraceID].Usage != before.Traces[initial.Scope.TraceID].Usage {
		t.Fatal("usage evidence changed the budget ledger")
	}
	stored, err := backend.Load(t.Context(), "session")
	if err != nil {
		t.Fatal(err)
	}
	last := stored.Commits[len(stored.Commits)-1]
	counts := map[string]int{}
	for _, r := range last.ControlRecords {
		counts[r.Type]++
	}
	want := map[string]int{"model_attempt_details": 1, "model_attempt_transition": 1, "invocation_message": 1, "tool_call": 2, "invocation": 1}
	if !reflect.DeepEqual(counts, want) || len(last.Entries) != 0 || after.LastSeq != before.LastSeq+1 {
		t.Fatal("child terminal did not commit all private acceptance facts together")
	}
	msg.Standard.Extra["private.fixture"].(map[string]any)["signature"] = "caller-mutation"
	msg.Standard.ContentBlocks[0].FunctionToolCall.Arguments = "changed"
	calls[0].Call.ProviderCallID = "changed"
	details.Usage[0].Snapshot.Usage.InputTotal.Value = 999
	copyView := m.View()
	copyView.InvocationMessages[initial.MessageID].Standard.Extra["private.fixture"].(map[string]any)["signature"] = "view-mutation"
	copyView.Invocations[inv.ID].MessageIDs[0] = "changed"
	copyView.AttemptDetails[terminal.UsageRef].Usage[0].Snapshot.Usage.InputTotal.Value = 999
	copyInvocation, ok := m.Invocation(inv.ID)
	if !ok {
		t.Fatal("missing invocation")
	}
	copyInvocation.MessageIDs[0], copyInvocation.CallIDs[0] = "changed", "changed"
	if !reflect.DeepEqual(after, m.View()) {
		t.Fatal("caller-owned message, invocation or evidence mutated committed state")
	}
	requireP2Code(t, m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "aborted", "", nil, nil), product.CodeStateConflict)
	if !reflect.DeepEqual(after, m.View()) {
		t.Fatal("duplicate terminal changed the accepted child")
	}
	reopened, err := state.NewManager(backend, "session")
	if err != nil || !reflect.DeepEqual(after, reopened.View()) {
		t.Fatal("load-only replay lost private child evidence", err)
	}
}

func TestP3ChildAttemptFailureOutcomesStayPrivateAndUnaccepted(t *testing.T) {
	for _, status := range []string{"failed", "incomplete", "aborted"} {
		for _, candidate := range []bool{false, true} {
			t.Run(status+map[bool]string{false: "-no-candidate", true: "-candidate"}[candidate], func(t *testing.T) {
				m, backend, initial, msg, _ := childAttemptFixture(t)
				msg.Status = agent.StatusIncomplete
				var private *agent.AgentMessage
				if candidate {
					private = &msg
				}
				before := m.View()
				if err := m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, status, "", private, nil); err != nil {
					t.Fatal(err)
				}
				after := m.View()
				requireChildParentUnchanged(t, before, after)
				requireChildSafeEvents(t, after.Events[len(before.Events):], initial, status)
				if !reflect.DeepEqual(before.Calls, after.Calls) || !reflect.DeepEqual(before.Invocations, after.Invocations) || after.AttemptResults[initial.ID].State != status || after.ModelAttempts[initial.ID] != initial {
					t.Fatal("failed child accepted calls/messages or overwrote initial")
				}
				if candidate && !reflect.DeepEqual(after.InvocationMessages[msg.ID], msg) || !candidate && len(after.InvocationMessages) != 0 {
					t.Fatal("incomplete child candidate was lost or fabricated")
				}
				reopened, err := state.NewManager(backend, "session")
				if err != nil || !reflect.DeepEqual(after, reopened.View()) {
					t.Fatal("child failure replay differs", err)
				}
			})
		}
	}
}

func TestP3ChildAttemptAppendFailurePublishesNothing(t *testing.T) {
	for _, status := range []string{"accepted", "failed", "incomplete", "aborted"} {
		t.Run(status, func(t *testing.T) {
			m, backend, initial, msg, calls := childAttemptFixture(t)
			if status != "accepted" {
				msg.Status, calls = agent.StatusIncomplete, nil
			}
			before := m.View()
			backend.fail = true
			err := m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, status, "", &msg, calls, childAttemptDetails(initial))
			if err == nil || !strings.Contains(err.Error(), "injected sync failure") || !reflect.DeepEqual(before, m.View()) {
				t.Fatal("failed append published child candidate, list, calls, terminal or event", err)
			}
			requireP2Code(t, m.WriteStatus(), product.CodeStorageUnavailable)
			reopened, err := state.NewManager(backend, "session")
			if err != nil || !reflect.DeepEqual(before, reopened.View()) || len(reopened.View().UnfinishedAttempts) != 1 {
				t.Fatal("reopen fabricated a child terminal after append failure", err)
			}
		})
	}
}

func TestP3ChildAttemptRejectsInvalidAcceptance(t *testing.T) {
	type inputs struct {
		scope  agent.ExecutionScope
		msg    agent.AgentMessage
		calls  []agent.ToolRecord
		status string
	}
	cases := []struct {
		name   string
		code   string
		change func(*inputs)
	}{
		{"candidate-id", product.CodeStateConflict, func(in *inputs) { in.msg.ID = "other" }},
		{"candidate-session", product.CodeStateConflict, func(in *inputs) { in.msg.Scope.SessionID = "other" }},
		{"candidate-trace", product.CodeStateConflict, func(in *inputs) { in.msg.Scope.TraceID = "other" }},
		{"candidate-invocation", product.CodeStateConflict, func(in *inputs) { in.msg.Scope.InvocationID = "other" }},
		{"candidate-turn", product.CodeStateConflict, func(in *inputs) { in.msg.Scope.TurnID = "parent-turn" }},
		{"candidate-tool-result", product.CodeStateConflict, func(in *inputs) { in.msg.Scope.ToolCallID = "delegate" }},
		{"candidate-input", product.CodeStateConflict, func(in *inputs) { in.msg.Scope.InputID = "parent-input" }},
		{"candidate-incomplete", product.CodeInvalidArgument, func(in *inputs) { in.msg.Status = agent.StatusIncomplete }},
		{"candidate-role", product.CodeInvalidArgument, func(in *inputs) { in.msg.Standard.Role = schema.AgenticRoleTypeUser }},
		{"candidate-kind", product.CodeInvalidArgument, func(in *inputs) { in.msg.Kind = agent.KindUser }},
		{"candidate-source", product.CodeInvalidArgument, func(in *inputs) { in.msg.Source.Kind = agent.SourceHuman }},
		{"call-provider", product.CodeStateConflict, func(in *inputs) { in.calls[0].Call.ProviderCallID = "other" }},
		{"call-name", product.CodeStateConflict, func(in *inputs) { in.calls[0].Call.Name = "other" }},
		{"call-arguments", product.CodeStateConflict, func(in *inputs) { in.calls[0].Call.Arguments = `{"child":false}` }},
		{"call-invalid-arguments", product.CodeStateConflict, func(in *inputs) {
			in.calls[0].Call.Arguments = "{"
			in.msg.Standard.ContentBlocks[0].FunctionToolCall.Arguments = "{"
		}},
		{"call-order", product.CodeStateConflict, func(in *inputs) {
			addSecondChildTool(&in.msg, &in.calls)
			in.calls[0], in.calls[1] = in.calls[1], in.calls[0]
		}},
		{"call-duplicate-product", product.CodeStateConflict, func(in *inputs) {
			addSecondChildTool(&in.msg, &in.calls)
			in.calls[1].Call.CallID = in.calls[0].Call.CallID
		}},
		{"call-duplicate-provider", product.CodeStateConflict, func(in *inputs) {
			addSecondChildTool(&in.msg, &in.calls)
			in.calls[1].Call.ProviderCallID = in.calls[0].Call.ProviderCallID
			in.msg.Standard.ContentBlocks[1].FunctionToolCall.CallID = in.calls[0].Call.ProviderCallID
		}},
		{"call-existing-product", product.CodeStateConflict, func(in *inputs) { in.calls[0].Call.CallID = "delegate" }},
		{"call-empty-product", product.CodeStateConflict, func(in *inputs) { in.calls[0].Call.CallID = "" }},
		{"call-operation", product.CodeStateConflict, func(in *inputs) { in.calls[0].Call.OperationID = "operation" }},
		{"call-claimed", product.CodeStateConflict, func(in *inputs) { in.calls[0].Claimed = true }},
		{"call-observation", product.CodeStateConflict, func(in *inputs) { in.calls[0].Observation = &agent.ToolObservation{Status: "succeeded"} }},
		{"call-selection", product.CodeStateConflict, func(in *inputs) { in.calls[0].Call.SelectionRevision = 1 }},
		{"call-session", product.CodeStateConflict, func(in *inputs) { in.calls[0].Scope.SessionID = "other" }},
		{"call-execution", product.CodeStateConflict, func(in *inputs) { in.calls[0].Scope.ExecutionID = "other" }},
		{"call-invocation", product.CodeStateConflict, func(in *inputs) { in.calls[0].Scope.InvocationID = "other" }},
		{"call-parent", product.CodeStateConflict, func(in *inputs) { in.calls[0].Scope.ParentInvocationID = "other" }},
		{"call-turn", product.CodeStateConflict, func(in *inputs) { in.calls[0].Scope.TurnID = "parent-turn" }},
		{"call-missing", product.CodeStateConflict, func(in *inputs) { in.calls = nil }},
		{"call-unmatched", product.CodeStateConflict, func(in *inputs) { in.msg.Standard.ContentBlocks = nil }},
		{"tool-block-missing", product.CodeStateConflict, func(in *inputs) { in.msg.Standard.ContentBlocks[0].FunctionToolCall = nil }},
		{"tool-block-wrong-type", product.CodeStateConflict, func(in *inputs) { in.msg.Standard.ContentBlocks[0].Type = schema.ContentBlockTypeAssistantGenText }},
		{"invalid-state", product.CodeStateConflict, func(in *inputs) { in.status = "complete" }},
		{"failed-tools", product.CodeInvalidArgument, func(in *inputs) { in.status = "failed"; in.msg.Status = agent.StatusIncomplete }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, backend, initial, msg, calls := childAttemptFixture(t)
			in := inputs{scope: initial.Scope, msg: msg, calls: calls, status: "accepted"}
			tc.change(&in)
			before := m.View()
			requireP2Code(t, m.SaveAttemptResult(t.Context(), in.scope, initial.ID, in.status, "", &in.msg, in.calls), tc.code)
			if !reflect.DeepEqual(before, m.View()) {
				t.Fatal("invalid child acceptance changed state")
			}
			reopened, err := state.NewManager(backend, "session")
			if err != nil || !reflect.DeepEqual(before, reopened.View()) {
				t.Fatal("invalid child acceptance reached storage", err)
			}
		})
	}
}

func TestP3ChildAttemptRejectsWrongPersistentScopeAndLateResults(t *testing.T) {
	for _, kind := range []string{"missing-invocation", "wrong-parent", "wrong-trace", "wrong-session", "child-turn", "child-selection", "interrupted", "terminal", "stopped-trace", "exit-proof"} {
		t.Run(kind, func(t *testing.T) {
			m, _, initial, msg, calls := childAttemptFixture(t)
			switch kind {
			case "missing-invocation":
				initial.Scope.InvocationID = "missing"
			case "wrong-parent":
				initial.Scope.ParentInvocationID = "wrong-parent"
			case "wrong-trace":
				initial.Scope.TraceID = "wrong-trace"
			case "wrong-session":
				initial.Scope.SessionID = "wrong-session"
			case "child-turn":
				initial.Scope.TurnID = "parent-turn"
			case "child-selection":
				initial.Scope.SelectionRevision = 1
			case "interrupted", "terminal":
				inv, _ := m.Invocation(initial.Scope.InvocationID)
				inv.State = "interrupted"
				if kind == "terminal" {
					inv.State = "completed"
				}
				if err := m.SaveInvocation(t.Context(), inv); err != nil {
					t.Fatal(err)
				}
			case "stopped-trace":
				if err := m.SetTraceState(t.Context(), initial.Scope.TraceID, "paused", false); err != nil {
					t.Fatal(err)
				}
			case "exit-proof":
				if err := m.ConfirmExecutionStopped(t.Context(), initial.Scope.TraceID); err != nil {
					t.Fatal(err)
				}
			}
			initial.ID, initial.MessageID, initial.ModelCallID = "invalid-attempt", "invalid-candidate", "invalid-model-call"
			msg.ID = initial.MessageID
			msg.Scope = agent.MessageScope{SessionID: initial.Scope.SessionID, TraceID: initial.Scope.TraceID, InvocationID: initial.Scope.InvocationID, TurnID: initial.Scope.TurnID}
			calls[0].Scope = initial.Scope
			if err := m.SaveRecords(t.Context(), m.View().LastSeq, state.Records{ModelAttempts: []state.ModelAttempt{initial}}); err != nil {
				t.Fatal(err)
			}
			before := m.View()
			requireP2Code(t, m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "accepted", "", &msg, calls), product.CodeStateConflict)
			if !reflect.DeepEqual(before, m.View()) {
				t.Fatal("late or wrong child scope accepted a result")
			}
		})
	}
}

func TestP3ChildAttemptReplayRejectsPartialAcceptance(t *testing.T) {
	for _, kind := range []string{"missing-message", "missing-call", "missing-invocation", "missing-transition", "missing-details", "orphan-message", "wrong-call-arguments", "wrong-call-provider", "reordered-calls", "wrong-message-scope", "incomplete-message", "message-record-id", "call-record-id", "wrong-message-list", "reordered-message-list", "wrong-call-list", "wrong-model-calls", "changed-parent", "failed-with-calls", "missing-safe-event", "private-public-event", "parent-entry"} {
		t.Run(kind, func(t *testing.T) {
			m, backend, initial, msg, calls := childAttemptFixture(t)
			addSecondChildTool(&msg, &calls)
			if err := m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "accepted", "tool_calls", &msg, calls); err != nil {
				t.Fatal(err)
			}
			corrupt := childResumeMutationStore{Store: backend, mutate: func(c *store.Commit) {
				var records []store.Record
				for _, r := range c.ControlRecords {
					if kind == "missing-message" && r.Type == "invocation_message" || kind == "missing-call" && r.Type == "tool_call" || kind == "missing-invocation" && r.Type == "invocation" || kind == "missing-transition" && r.Type == "model_attempt_transition" || kind == "missing-details" && r.Type == "model_attempt_details" || kind == "orphan-message" && r.Type != "invocation_message" {
						continue
					}
					switch r.Type {
					case "invocation_message":
						var candidate agent.AgentMessage
						_ = json.Unmarshal(r.Payload, &candidate)
						if kind == "wrong-message-scope" {
							candidate.Scope.InvocationID = "other"
						}
						if kind == "incomplete-message" {
							candidate.Status = agent.StatusIncomplete
						}
						if kind == "message-record-id" {
							r.ID = "other"
						}
						r.Payload, _ = json.Marshal(candidate)
					case "tool_call":
						var call agent.ToolRecord
						_ = json.Unmarshal(r.Payload, &call)
						if kind == "wrong-call-arguments" {
							call.Call.Arguments = `{"changed":true}`
						}
						if kind == "wrong-call-provider" {
							call.Call.ProviderCallID = "wrong"
						}
						if kind == "call-record-id" {
							r.ID = "wrong"
						}
						r.Payload, _ = json.Marshal(call)
					case "invocation":
						var inv state.Invocation
						_ = json.Unmarshal(r.Payload, &inv)
						if kind == "wrong-message-list" {
							inv.MessageIDs = nil
						}
						if kind == "reordered-message-list" {
							inv.MessageIDs = []string{"other", initial.MessageID}
						}
						if kind == "wrong-call-list" {
							inv.CallIDs[0], inv.CallIDs[1] = inv.CallIDs[1], inv.CallIDs[0]
						}
						if kind == "wrong-model-calls" {
							inv.ModelCalls++
						}
						if kind == "changed-parent" {
							inv.ParentInvocationID = "other"
						}
						r.Payload, _ = json.Marshal(inv)
					case "model_attempt_transition":
						var result state.ModelAttemptTransition
						_ = json.Unmarshal(r.Payload, &result)
						if kind == "failed-with-calls" {
							result.State = "failed"
						}
						r.Payload, _ = json.Marshal(result)
					}
					records = append(records, r)
				}
				if kind == "reordered-calls" {
					var indexes []int
					for i, r := range records {
						if r.Type == "tool_call" {
							indexes = append(indexes, i)
						}
					}
					records[indexes[0]], records[indexes[1]] = records[indexes[1]], records[indexes[0]]
				}
				c.ControlRecords = records
				if kind == "missing-safe-event" {
					c.Events = nil
				}
				if kind == "private-public-event" {
					c.Events[0].Payload, _ = json.Marshal(msg)
				}
				if kind == "parent-entry" {
					c.Entries = []store.Record{{Type: "message", Version: 1, ID: msg.ID, ParentID: m.View().LeafID, Payload: func() []byte { raw, _ := json.Marshal(msg); return raw }()}}
				}
			}}
			if reopened, err := state.NewManager(corrupt, "session"); err == nil || reopened != nil {
				t.Fatal("partial or modified child acceptance became valid on load-only replay")
			}
		})
	}
}

func TestP3ChildAttemptAcceptedMessageOrderOwnedAcrossInvocationTransitions(t *testing.T) {
	m, backend, initial, msg, calls := childAttemptFixture(t)
	if err := m.SaveAttemptResult(t.Context(), initial.Scope, initial.ID, "accepted", "tool_calls", &msg, calls); err != nil {
		t.Fatal(err)
	}
	second := initial
	second.ID, second.MessageID, second.ModelCallID = "second-attempt", "second-candidate", "second-model-call"
	msg.ID, msg.Standard.ContentBlocks = second.MessageID, nil
	if err := m.SaveRecords(t.Context(), m.View().LastSeq, state.Records{ModelAttempts: []state.ModelAttempt{second}}); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveAttemptResult(t.Context(), second.Scope, second.ID, "accepted", "stop", &msg, nil); err != nil {
		t.Fatal(err)
	}
	inv, _ := m.Invocation(initial.Scope.InvocationID)
	wantMessages := []string{initial.MessageID, second.MessageID}
	if !reflect.DeepEqual(inv.MessageIDs, wantMessages) {
		t.Fatal("accepted message order was lost")
	}
	inv.State, inv.MessageIDs, inv.CallIDs = "interrupted", []string{"caller-forged"}, []string{"caller-forged"}
	if err := m.SaveInvocation(t.Context(), inv); err != nil {
		t.Fatal(err)
	}
	inv, _ = m.Invocation(inv.ID)
	if !reflect.DeepEqual(inv.MessageIDs, wantMessages) || !reflect.DeepEqual(inv.CallIDs, []string{calls[0].Call.CallID}) {
		t.Fatal("SaveInvocation overwrote acceptance-owned lists")
	}
	inv.State = "completed"
	if err := m.SaveInvocation(t.Context(), inv); err != nil {
		t.Fatal(err)
	}
	before := m.View()
	inv.MessageIDs = nil
	requireP2Code(t, m.SaveInvocation(t.Context(), inv), product.CodeStateConflict)
	if !reflect.DeepEqual(before, m.View()) {
		t.Fatal("terminal invocation was mutable")
	}
	reopened, err := state.NewManager(backend, "session")
	if err != nil || !reflect.DeepEqual(before, reopened.View()) {
		t.Fatal("invocation transition replay differs", err)
	}
}

func TestP3ChildAttemptLegacyChildCallsRemainReplayable(t *testing.T) {
	m, backend, initial, _, calls := childAttemptFixture(t)
	if err := m.RegisterChildCalls(t.Context(), initial.Scope.InvocationID, calls); err != nil {
		t.Fatal(err)
	}
	before := m.View()
	if len(before.InvocationMessages) != 0 || len(before.Invocations[initial.Scope.InvocationID].MessageIDs) != 0 || len(before.AttemptResults) != 0 {
		t.Fatal("legacy child call acceptance gained model acceptance permission")
	}
	if err := m.RegisterChildCalls(t.Context(), initial.Scope.InvocationID, calls); err == nil {
		t.Fatal("legacy duplicate call accepted")
	}
	reopened, err := state.NewManager(backend, "session")
	if err != nil || !reflect.DeepEqual(before, reopened.View()) {
		t.Fatal("legacy child call replay differs", err)
	}
}
