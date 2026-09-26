package sessions

import (
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

func TestRestoreResourceHoldsFailurePreservesPriorHoldsAndReleases(t *testing.T) {
	const sessionID = "atomic-restore"
	backend, err := memory.Open(sessionID, store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	manager, err := state.NewManager(backend, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	receipt, err := manager.Accept(ctx, agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run"}`)}, agent.TargetAgent{Name: "main", Version: "main-v1", Generation: "gen"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetTraceState(ctx, receipt.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	trace := manager.View().Traces[receipt.TraceID]
	scope := agent.ExecutionScope{SessionID: sessionID, TraceID: receipt.TraceID, InvocationID: trace.InvocationID, TurnID: "turn", ExecutionID: "exec", Generation: "gen"}
	if err := manager.SaveTurn(ctx, agent.TurnRecord{ID: scope.TurnID, TraceID: scope.TraceID, InvocationID: scope.InvocationID}); err != nil {
		t.Fatal(err)
	}
	calls := make([]agent.ToolRecord, 0, 3)
	blocks := make([]*schema.ContentBlock, 0, 3)
	for _, id := range []string{"known", "valid", "invalid"} {
		calls = append(calls, agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: id, ProviderCallID: id, Name: "work", Arguments: `{}`, Generation: "gen", Hash: id}})
		blocks = append(blocks, schema.NewContentBlock(&schema.FunctionToolCall{CallID: id, Name: "work", Arguments: `{}`}))
	}
	msg := agent.AgentMessage{ID: "assistant", Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: sessionID, TraceID: scope.TraceID, TurnID: scope.TurnID, InvocationID: scope.InvocationID}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: blocks}}
	if err := manager.SaveAssistant(ctx, msg, calls); err != nil {
		t.Fatal(err)
	}
	usage := trace.Usage
	for _, call := range calls {
		usage.ToolExecutions++
		if err := manager.ClaimTool(ctx, call.Call, usage); err != nil {
			t.Fatal(err)
		}
	}
	known := manager.View().Calls["known"]
	known.Observation = &agent.ToolObservation{Status: "succeeded", SideEffect: "none", Executed: true}
	if err := manager.SaveCall(ctx, known); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, resource string }{{"valid", "ok"}, {"invalid", "../escape"}} {
		frozen := agent.FrozenExecution{ID: "execution:" + item.id, CallID: item.id, Scope: scope, Resources: []agent.ExecutionResource{{Identity: item.resource}}, Effect: "write", Concurrency: "exclusive"}
		frozen.Hash, err = frozen.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.SaveRecords(ctx, manager.View().LastSeq, state.Records{FrozenExecutions: []state.FrozenExecution{frozen}}); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 0; attempt < 100; attempt++ {
		scheduler := tools.NewResourceScheduler()
		request := tools.ResourceRequest{Environment: "memory", Workspace: "workspace", Effect: "unknown"}
		knownID := tools.ResourceHoldID(sessionID, "known")
		if err := scheduler.RestoreHold(knownID, request); err != nil {
			t.Fatal(err)
		}
		rt := &runtime{opts: Options{SessionID: sessionID, Workspace: "workspace", ResourceScheduler: scheduler, ResourceEnvironment: "memory"}, manager: manager}
		if err := rt.restoreResourceHolds(); err == nil {
			t.Fatal("invalid later hold accepted")
		}
		if !scheduler.HasHold(knownID) {
			t.Fatal("known hold released despite failed restore")
		}
		if scheduler.HasHold(tools.ResourceHoldID(sessionID, "valid")) {
			t.Fatal("partial hold exposed despite failed restore")
		}
	}
}
