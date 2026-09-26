package state_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func TestP2AttemptTerminalAppendIsAtomicAndReplayable(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "append_failure"}[fail], func(t *testing.T) {
			m, backend := fixture(t)
			scope := agent.ExecutionScope{SessionID: "session", TraceID: "trace", TurnID: "turn", InvocationID: "inv", ExecutionID: "exec"}
			if err := m.SaveTurn(t.Context(), agent.TurnRecord{ID: "turn", TraceID: "trace", InvocationID: "inv"}); err != nil {
				t.Fatal(err)
			}
			initial := state.ModelAttempt{ID: "attempt", ModelCallID: "turn", MessageID: "candidate", StreamID: "stream", Scope: scope, State: "started"}
			if err := m.SaveRecords(t.Context(), m.View().LastSeq, state.Records{ModelAttempts: []state.ModelAttempt{initial}}); err != nil {
				t.Fatal(err)
			}
			msg := agent.AgentMessage{ID: "candidate", Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: "session", TraceID: "trace", TurnID: "turn", InvocationID: "inv"}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant}}
			msg.Standard.ContentBlocks = []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider", Name: "write", Arguments: "{}"})}
			calls := []agent.ToolRecord{{Scope: scope, Call: agent.FrozenCall{CallID: "call", ProviderCallID: "provider", Name: "write", Arguments: "{}"}}}
			details := agent.ModelAttemptDetails{Usage: []agent.ModelRequestUsage{{Request: llm.TransportRequest{ModelCallID: "turn", AttemptID: "attempt", TransportAttempt: 1, Purpose: "agent"}, Snapshot: llm.UsageSnapshot{Usage: llm.UsageRecord{InputTotal: llm.UsageValue{Known: true, Value: 12, Source: "fixture"}}}}}}
			before := m.View()
			backend.fail = fail
			err := m.SaveAttemptResult(t.Context(), scope, "attempt", "accepted", "stop", &msg, calls, details)
			if fail {
				if err == nil || !reflect.DeepEqual(before, m.View()) {
					t.Fatal("failed append published attempt or candidate")
				}
				reopened, e := state.NewManager(backend, "session")
				if e != nil {
					t.Fatal(e)
				}
				if len(reopened.View().AttemptResults) != 0 || reopened.View().ModelAttempts["attempt"] != initial {
					t.Fatal("reopen fabricated durable terminal")
				}
				raw, _ := json.Marshal(reopened.View())
				var diagnostic map[string]any
				_ = json.Unmarshal(raw, &diagnostic)
				unfinished, _ := diagnostic["UnfinishedAttempts"].([]any)
				if len(unfinished) != 1 {
					t.Error("reopened attempt has no explicit unfinished diagnostic")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			after := m.View()
			details.Usage[0].Snapshot.Usage.InputTotal.Value = 999
			msg.Standard.ContentBlocks[0].FunctionToolCall.Name = "mutated"
			calls[0].Call.Name = "mutated"
			copyView := m.View()
			ref := copyView.AttemptResults["attempt"].UsageRef
			copyView.AttemptDetails[ref].Usage[0].Snapshot.Usage.InputTotal.Value = 999
			copyView.Messages[0].Standard.ContentBlocks[0].FunctionToolCall.Name = "mutated"
			if !reflect.DeepEqual(after, m.View()) {
				t.Fatal("caller mutation changed durable candidate or evidence")
			}
			if len(after.AttemptDetails) != 1 || len(after.Calls) != 1 || len(after.UnfinishedAttempts) != 0 {
				t.Fatal("acceptance missing atomic evidence or calls")
			}
			stored, e := backend.Load(t.Context(), "session")
			if e != nil {
				t.Fatal(e)
			}
			last := stored.Commits[len(stored.Commits)-1]
			kinds := map[string]bool{}
			for _, r := range last.ControlRecords {
				kinds[r.Type] = true
			}
			if len(last.Entries) != 1 || !kinds["model_attempt_details"] || !kinds["model_attempt_transition"] || !kinds["tool_call"] {
				t.Fatal("candidate, terminal, evidence and tool were not one commit")
			}
			if after.ModelAttempts["attempt"] != initial || after.AttemptResults["attempt"].Revision != 2 || after.AttemptResults["attempt"].ExpectedRevision != 1 || len(after.Messages) != 1 {
				t.Fatal("attempt transition overwrote initial record or lost candidate")
			}
			requireP2Code(t, m.SaveAttemptResult(t.Context(), scope, "attempt", "failed", "", nil, nil), product.CodeStateConflict)
			if !reflect.DeepEqual(after, m.View()) {
				t.Fatal("duplicate terminal changed state")
			}
			reopened, e := state.NewManager(backend, "session")
			if e != nil || !reflect.DeepEqual(after, reopened.View()) {
				t.Fatal("terminal replay differs", e)
			}
		})
	}
}
