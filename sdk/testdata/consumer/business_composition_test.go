package consumer_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

// The examples below compose two independently controlled factories only in
// ordinary business ToolDefinition.Run closures. Neither factory is an agent
// target, and no authorization, budget, history or execution state is shared.
type businessModel struct {
	calls        atomic.Int32
	paired       atomic.Int32
	tool, callID string
	arguments    string
	final        string
	check        func(string) error
	entered      chan struct{}
	release      <-chan struct{}
	cancelled    chan struct{}
}

func (*businessModel) Configuration() sdk.ModelConfig {
	return sdk.ModelConfig{Version: "business-composition-model-v1"}
}

func (m *businessModel) Generate(ctx context.Context, input []*schema.AgenticMessage, _ ...einomodel.Option) (*schema.AgenticMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n := m.calls.Add(1)
	if m.entered != nil {
		close(m.entered)
		select {
		case <-m.release:
		case <-ctx.Done():
			close(m.cancelled)
			<-m.release
			return nil, ctx.Err()
		}
	}
	if m.tool != "" && n == 1 {
		return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant,
			ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: m.callID, Name: m.tool, Arguments: m.arguments})},
			Extra:         map[string]any{"seasprak.finish": "tool_calls"}}, nil
	}
	want := int32(1)
	if m.tool != "" {
		want = 2
		count, text := businessResult(input, m.callID)
		m.paired.Store(int32(count))
		if count != 1 {
			return nil, fmt.Errorf("business tool has %d results for provider call %s, want one", count, m.callID)
		}
		if m.check != nil {
			if err := m.check(text); err != nil {
				return nil, err
			}
		}
	}
	if n != want {
		return nil, errors.New("unexpected business model invocation")
	}
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: m.final})},
		Extra:         map[string]any{"seasprak.finish": "stop"}}, nil
}

func (m *businessModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	message, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{message}), nil
}

func businessResult(messages []*schema.AgenticMessage, callID string) (int, string) {
	count, text := 0, ""
	for _, message := range messages {
		for _, block := range message.ContentBlocks {
			if result := block.FunctionToolResult; result != nil && result.CallID == callID {
				count++
				for _, part := range result.Content {
					if part.Text != nil {
						text += part.Text.Text
					}
				}
			}
		}
	}
	return count, text
}

func businessContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func businessClose(closeFn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return closeFn(ctx)
}

func businessCleanup(t *testing.T, closeFn func(context.Context) error) {
	t.Helper()
	t.Cleanup(func() {
		if err := businessClose(closeFn); err != nil {
			t.Errorf("business instance cleanup: %v", err)
		}
	})
}

func businessReceive[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatalf("business execution observation timed out: %v", ctx.Err())
		var zero T
		return zero
	}
}

func businessWaitCode(ctx context.Context, s *sdk.AgentSession, traceID string) (sdk.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		snap, err := s.Snapshot(ctx)
		if err != nil {
			return snap, err
		}
		if tr := snap.Traces[traceID]; tr != nil && tr.ExecutionStopped && (tr.Settled || tr.State == "paused") {
			return snap, nil
		}
		select {
		case <-ctx.Done():
			return snap, ctx.Err()
		case <-tick.C:
		}
	}
}

func businessWaitWorkflow(ctx context.Context, w *sdk.WorkflowAgent) (sdk.WorkflowSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		snap, err := w.Snapshot(ctx)
		if err != nil {
			return snap, err
		}
		if snap.ExecutionStopped && (snap.State == "completed" || snap.State == "failed" || snap.State == "paused" || snap.State == "cancelled") {
			return snap, nil
		}
		select {
		case <-ctx.Done():
			return snap, ctx.Err()
		case <-tick.C:
		}
	}
}

func businessTool(name string, run func(context.Context, json.RawMessage) (string, error)) sdk.ToolDefinition {
	return sdk.ToolDefinition{Name: name, Version: "v1", Description: "independently controlled business operation",
		Schema:    json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`),
		Execution: sdk.ExecutionDescription{Effect: "read", BackendID: "trusted-run"}, Run: run}
}

func businessCodeOptions(t *testing.T, id string, model sdk.Model, tools ...sdk.ToolDefinition) sdk.SessionOptions {
	t.Helper()
	return sdk.SessionOptions{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: id,
		Profile: sdk.ProfileMemory, Principal: id + "-owner", Model: model, Tools: tools,
		GenerationFingerprint: "business-composition-code-v1", Limits: sdk.DefaultLimits()}
}

func businessWorkflowOptions(t *testing.T, id string, tool sdk.ToolDefinition) sdk.WorkflowOptions {
	t.Helper()
	def := sdk.WorkflowDefinition{Name: "business-" + id, Version: "v1", Source: "consumer", FormatVersion: sdk.WorkflowFormatV1, Resumable: true,
		InputSchema: tool.Schema,
		Nodes: []sdk.WorkflowNode{{ID: "s", Type: "start"},
			{ID: "t", Type: "tool", Tool: tool.Name, Inputs: map[string]sdk.WorkflowValue{"name": {Ref: &sdk.WorkflowRef{Node: "s", Field: "name"}}}},
			{ID: "e", Type: "end", Inputs: map[string]sdk.WorkflowValue{"result": {Ref: &sdk.WorkflowRef{Node: "t", Field: "result"}}}}},
		Edges: []sdk.WorkflowEdge{{From: "s", To: "t"}, {From: "t", To: "e"}}}
	return sdk.WorkflowOptions{Workspace: t.TempDir(), StateRoot: t.TempDir(), RunID: id, Definition: def,
		Tools: []sdk.ToolDefinition{tool}, Principal: id + "-owner", GenerationFingerprint: "business-composition-workflow-v1", Limits: sdk.DefaultLimits()}
}

func businessUsage(t *testing.T, label string, usage sdk.Usage, models, tools int) {
	t.Helper()
	if usage.LogicalModelCalls != models || usage.TransportRequests != models || usage.ToolExecutions != tools {
		t.Fatalf("%s usage models/transport/tools=%d/%d/%d, want %d/%d/%d", label, usage.LogicalModelCalls, usage.TransportRequests, usage.ToolExecutions, models, models, tools)
	}
}

func businessCodeResult(t *testing.T, snap sdk.Snapshot, providerID, status, effect string, executed bool) sdk.ToolRecord {
	t.Helper()
	if len(snap.Calls) != 1 || len(snap.Observations) != 1 {
		t.Fatalf("outer accepted calls/observations=%d/%d, want 1/1", len(snap.Calls), len(snap.Observations))
	}
	var call sdk.ToolRecord
	for _, call = range snap.Calls {
	}
	if call.Call.ProviderCallID != providerID || call.Scope.SessionID != snap.SessionID || call.Scope.WorkflowRunID != "" || call.Call.CallID == providerID || call.Observation == nil || call.Observation.Status != status || call.Observation.SideEffect != effect || call.Observation.Executed != executed || call.Claimed != executed {
		t.Fatalf("business tool result has wrong identity or observation: call=%+v", call)
	}
	var messages []*schema.AgenticMessage
	for _, message := range snap.Messages {
		if message.Kind == sdk.KindToolResult && message.Standard != nil {
			messages = append(messages, message.Standard)
		}
	}
	if count, _ := businessResult(messages, providerID); count != 1 {
		t.Fatalf("saved paired results=%d, want one for %s", count, providerID)
	}
	for _, observed := range snap.Observations {
		if observed.CallID != call.Call.CallID || observed.Observation.Status != status || observed.Observation.SideEffect != effect || observed.Observation.Executed != executed {
			t.Fatalf("business failure/success observation is not paired: %+v", observed)
		}
	}
	return call
}

func businessWorkflowResult(t *testing.T, snap sdk.WorkflowSnapshot, status, effect string, executed bool) sdk.WorkflowRunNode {
	t.Helper()
	if len(snap.WorkflowNodes) != 1 || len(snap.Observations) != 1 {
		t.Fatalf("workflow business nodes/observations=%d/%d, want 1/1", len(snap.WorkflowNodes), len(snap.Observations))
	}
	var node sdk.WorkflowRunNode
	for _, node = range snap.WorkflowNodes {
	}
	observation, ok := snap.Observations[node.ToolCallID]
	if !ok || node.Kind != "tool" || node.ToolCallID != node.ID || observation.NodeExecutionID != node.ID || observation.ToolCallID != node.ID || observation.Status != status || observation.SideEffect != effect || observation.Executed != executed {
		t.Fatalf("workflow business result is not paired: node=%+v observation=%+v", node, observation)
	}
	return node
}

func businessJournals(t *testing.T, code sdk.SessionOptions, workflow sdk.WorkflowOptions) {
	t.Helper()
	if code.Workspace == workflow.Workspace || code.StateRoot == workflow.StateRoot || code.SessionID == workflow.RunID {
		t.Fatal("business instances share a workspace, state root or root identity")
	}
	for _, item := range []struct {
		path, kind, id string
	}{
		{filepath.Join(code.StateRoot, "sessions", code.SessionID, "journal.jsonl"), "code", code.SessionID},
		{filepath.Join(workflow.StateRoot, "workflow-runs", workflow.RunID, "journal.jsonl"), "workflow", workflow.RunID},
	} {
		file, err := os.Open(item.path)
		if err != nil {
			t.Fatal(err)
		}
		scan := bufio.NewScanner(file)
		if !scan.Scan() {
			_ = file.Close()
			t.Fatalf("business journal has no header: %v", scan.Err())
		}
		var header sdk.Header
		err = json.Unmarshal(scan.Bytes(), &header)
		_ = file.Close()
		if err != nil || header.RecordType != "header" || header.ResourceID() != item.id || item.kind == "workflow" && (string(header.ResourceType) != "workflow" || header.SessionID != "") || item.kind == "code" && (header.RunID != "" || string(header.ResourceType) != "" && string(header.ResourceType) != "code") {
			t.Fatalf("business journal type/owner mismatch: kind=%s header=%+v err=%v", item.kind, header, err)
		}
	}
}

// Read only the acknowledged journal prefix. Stop at the snapshot's revision
// rather than reading a possible concurrent append beyond that prefix.
func businessJournalThrough(t *testing.T, path string, revision uint64) []sdk.Commit {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 4096), sdk.DefaultLimits().MaxCommitLineBytes)
	if !scan.Scan() {
		t.Fatalf("business journal header missing: %v", scan.Err())
	}
	var commits []sdk.Commit
	for scan.Scan() {
		var commit sdk.Commit
		if err := json.Unmarshal(scan.Bytes(), &commit); err != nil {
			t.Fatalf("business journal commit decode: %v", err)
		}
		commits = append(commits, commit)
		if commit.CommitSeq == revision {
			return commits
		}
		if commit.CommitSeq > revision {
			t.Fatalf("business journal skipped snapshot revision %d", revision)
		}
	}
	t.Fatalf("business journal did not reach revision %d: %v", revision, scan.Err())
	return nil
}

func TestBusinessCompositionCodeToWorkflow(t *testing.T) {
	ctx := businessContext(t)
	var factories, tools atomic.Int32
	innerScope := make(chan sdk.ExecutionScope, 1)
	innerModel := &businessModel{final: "must never execute"}
	innerOptions := businessWorkflowOptions(t, "business-cw-inner", businessTool("lookup", func(ctx context.Context, args json.RawMessage) (string, error) {
		tools.Add(1)
		innerScope <- sdk.ScopeFromContext(ctx, sdk.ExecutionScope{})
		if string(args) != `{"name":"parcel"}` {
			return "", errors.New("structured workflow input changed")
		}
		return "selected-workflow-value", nil
	}))
	innerOptions.Models = map[string]einomodel.AgenticModel{"unused": innerModel}
	innerOptions.Limits.TraceToolCalls = 1
	type proof struct{ initial, final sdk.WorkflowSnapshot }
	finished := make(chan proof, 1)
	outerScope := make(chan sdk.ExecutionScope, 1)
	outerTool := businessTool("run_workflow", func(toolctx context.Context, args json.RawMessage) (output string, err error) {
		outerScope <- sdk.ScopeFromContext(toolctx, sdk.ExecutionScope{})
		factories.Add(1)
		inner, err := sdk.CreateWorkflowAgent(toolctx, innerOptions)
		if err != nil {
			return "", err
		}
		defer func() { err = errors.Join(err, businessClose(inner.Close)) }()
		initial, err := inner.Snapshot(toolctx)
		if err != nil {
			return "", err
		}
		sub, err := inner.SubscribeFrom(toolctx, sdk.WorkflowSubscribeOptions{After: initial.DurableSeq})
		if err != nil {
			return "", err
		}
		defer sub.Close()
		if _, err = inner.SubmitInput(toolctx, sdk.WorkflowInputCommand{Input: args, IdempotencyKey: "inner-input", Principal: innerOptions.Principal}); err != nil {
			return "", err
		}
		final, err := businessWaitWorkflow(toolctx, inner)
		if err != nil {
			return "", err
		}
		eventctx, cancel := context.WithTimeout(toolctx, 5*time.Second)
		defer cancel()
		for {
			select {
			case ev, ok := <-sub.Events:
				if !ok || ev.Scope.WorkflowRunID != innerOptions.RunID || ev.Scope.SessionID != "" || ev.Scope.TraceID != "" || ev.Scope.TurnID != "" {
					return "", sdk.NewError(sdk.CodeStateConflict, "inner workflow event has an incompatible execution root")
				}
				if ev.DurableSeq != nil && *ev.DurableSeq == final.DurableSeq {
					finished <- proof{initial, final}
					if final.State != "completed" {
						return "", sdk.NewError(final.ErrorCode, "independent workflow did not complete")
					}
					return string(final.Result), nil
				}
			case <-eventctx.Done():
				return "", eventctx.Err()
			}
		}
	})
	model := &businessModel{tool: "run_workflow", callID: "business-cw-provider", arguments: `{"name":"parcel"}`, final: "outer-code-finished", check: func(result string) error {
		if result != `{"result":"selected-workflow-value"}` {
			return errors.New("outer model did not receive only the selected workflow end result")
		}
		return nil
	}}
	outerOptions := businessCodeOptions(t, "business-cw-outer", model, outerTool)
	outerOptions.Limits.TraceLogicalModelCalls, outerOptions.Limits.TraceTransportRequests, outerOptions.Limits.TraceToolCalls = 2, 2, 1
	outer, err := sdk.CreateAgentSession(ctx, outerOptions)
	if err != nil {
		t.Fatal(err)
	}
	businessCleanup(t, outer.Close)
	initial, err := outer.Snapshot(ctx)
	if err != nil || len(initial.Traces) != 0 || model.calls.Load() != 0 || factories.Load() != 0 || tools.Load() != 0 {
		t.Fatalf("outer factory executed business work: err=%v", err)
	}
	sub := outer.SubscribeEvents(sdk.DefaultLimits())
	defer sub.Close()
	input, err := outer.SubmitInput(ctx, sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run workflow business operation"}`), IdempotencyKey: "outer-input"})
	if err != nil {
		t.Fatal(err)
	}
	final, err := businessWaitCode(ctx, outer, input.TraceID)
	if err != nil || final.Traces[input.TraceID].State != "completed" || !final.Traces[input.TraceID].Settled {
		t.Fatalf("outer Code execution: err=%v trace=%+v", err, final.Traces[input.TraceID])
	}
	inner := businessReceive(t, ctx, finished)
	if inner.initial.State != "created" || !inner.initial.ExecutionStopped || len(inner.initial.WorkflowNodes) != 0 || inner.final.RunID != innerOptions.RunID || !inner.final.ExecutionStopped || inner.final.InstanceID == "" {
		t.Fatalf("independent workflow factory/run identity changed: initial=%+v final=%+v", inner.initial, inner.final)
	}
	businessUsage(t, "created Workflow", inner.initial.Usage, 0, 0)
	businessUsage(t, "outer Code", final.Traces[input.TraceID].Usage, 2, 1)
	businessUsage(t, "inner Workflow", inner.final.Usage, 0, 1)
	call := businessCodeResult(t, final, model.callID, "succeeded", "none", true)
	node := businessWorkflowResult(t, inner.final, "succeeded", "none", true)
	codeScope, workflowScope := businessReceive(t, ctx, outerScope), businessReceive(t, ctx, innerScope)
	if codeScope.SessionID != outerOptions.SessionID || codeScope.TraceID != input.TraceID || codeScope.WorkflowRunID != "" || codeScope.InvocationID != call.Scope.InvocationID || workflowScope.WorkflowRunID != innerOptions.RunID || workflowScope.SessionID != "" || workflowScope.TraceID != "" || workflowScope.TurnID != "" || workflowScope.NodeExecutionID != node.ID || workflowScope.InvocationID == codeScope.InvocationID {
		t.Fatalf("business roots were mixed: code=%+v workflow=%+v", codeScope, workflowScope)
	}
	if model.calls.Load() != 2 || model.paired.Load() != 1 || factories.Load() != 1 || tools.Load() != 1 || innerModel.calls.Load() != 0 || len(inner.final.ModelAttempts) != 0 || len(final.Invocations) != 0 {
		t.Fatalf("business counts models/paired/factories/tools/innerModels=%d/%d/%d/%d/%d", model.calls.Load(), model.paired.Load(), factories.Load(), tools.Load(), innerModel.calls.Load())
	}
	for {
		ev := businessReceive(t, ctx, sub.Events)
		if ev.Scope.SessionID != outerOptions.SessionID || ev.Scope.WorkflowRunID != "" {
			t.Fatalf("inner workflow event leaked into outer Code subscription: %+v", ev.Scope)
		}
		if ev.Type == "trace.settled" && ev.Scope.TraceID == input.TraceID {
			break
		}
	}
	businessJournals(t, outerOptions, innerOptions)
}

func TestBusinessCompositionWorkflowToCode(t *testing.T) {
	ctx := businessContext(t)
	model := &businessModel{final: "selected-code-answer"}
	innerOptions := businessCodeOptions(t, "business-wc-inner", model)
	innerOptions.Limits.TraceLogicalModelCalls, innerOptions.Limits.TraceTransportRequests = 1, 1
	var factories, tools atomic.Int32
	type proof struct {
		initial, final sdk.Snapshot
		input          sdk.InputReceipt
	}
	finished := make(chan proof, 1)
	outerScope := make(chan sdk.ExecutionScope, 1)
	outerTool := businessTool("run_code", func(toolctx context.Context, args json.RawMessage) (output string, err error) {
		tools.Add(1)
		outerScope <- sdk.ScopeFromContext(toolctx, sdk.ExecutionScope{})
		var input struct {
			Name string `json:"name"`
		}
		if err = json.Unmarshal(args, &input); err != nil {
			return "", err
		}
		factories.Add(1)
		inner, err := sdk.CreateAgentSession(toolctx, innerOptions)
		if err != nil {
			return "", err
		}
		defer func() { err = errors.Join(err, businessClose(inner.Close)) }()
		initial, err := inner.Snapshot(toolctx)
		if err != nil {
			return "", err
		}
		if len(initial.Traces) != 0 || model.calls.Load() != 0 {
			return "", sdk.NewError(sdk.CodeStateConflict, "independent Code factory executed before input")
		}
		sub := inner.SubscribeEvents(sdk.DefaultLimits())
		defer sub.Close()
		prompt, _ := json.Marshal(map[string]string{"text": input.Name})
		receipt, err := inner.SubmitInput(toolctx, sdk.InputCommand{Kind: "prompt", Content: prompt, Principal: innerOptions.Principal, IdempotencyKey: "inner-input"})
		if err != nil {
			return "", err
		}
		final, err := businessWaitCode(toolctx, inner, receipt.TraceID)
		if err != nil {
			return "", err
		}
		eventctx, cancel := context.WithTimeout(toolctx, 5*time.Second)
		defer cancel()
		settled := false
		for !settled {
			select {
			case ev, ok := <-sub.Events:
				if !ok || ev.Scope.SessionID != innerOptions.SessionID || ev.Scope.WorkflowRunID != "" {
					return "", sdk.NewError(sdk.CodeStateConflict, "inner Code event has an incompatible execution root")
				}
				settled = ev.Type == "trace.settled" && ev.Scope.TraceID == receipt.TraceID
			case <-eventctx.Done():
				return "", eventctx.Err()
			}
		}
		finished <- proof{initial, final, receipt}
		if final.Traces[receipt.TraceID].State != "completed" {
			return "", sdk.NewError(sdk.CodeResourceUnavailable, "independent Code execution did not complete")
		}
		// Select only this trace's final assistant answer, not its chat history.
		for i := len(final.Messages) - 1; i >= 0; i-- {
			message := final.Messages[i]
			if message.Kind == sdk.KindAssistant && message.Scope.TraceID == receipt.TraceID && message.Standard != nil && message.Status == sdk.StatusComplete {
				text := ""
				for _, block := range message.Standard.ContentBlocks {
					if block.AssistantGenText != nil {
						text += block.AssistantGenText.Text
					}
				}
				return text, nil
			}
		}
		return "", sdk.NewError(sdk.CodeStateConflict, "independent Code final answer is unavailable")
	})
	outerOptions := businessWorkflowOptions(t, "business-wc-outer", outerTool)
	outerOptions.Limits.TraceToolCalls = 1
	outerModel := &businessModel{final: "must never execute"}
	outerOptions.Models = map[string]einomodel.AgenticModel{"unused": outerModel}
	outer, err := sdk.CreateWorkflowAgent(ctx, outerOptions)
	if err != nil {
		t.Fatal(err)
	}
	businessCleanup(t, outer.Close)
	initial, err := outer.Snapshot(ctx)
	if err != nil || initial.State != "created" || factories.Load() != 0 || tools.Load() != 0 || model.calls.Load() != 0 {
		t.Fatalf("Workflow factory executed business work: err=%v", err)
	}
	businessUsage(t, "created outer Workflow", initial.Usage, 0, 0)
	sub, err := outer.SubscribeFrom(ctx, sdk.WorkflowSubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	_, err = outer.SubmitInput(ctx, sdk.WorkflowInputCommand{Input: json.RawMessage(`{"name":"inner-code-prompt-only"}`), Principal: outerOptions.Principal, IdempotencyKey: "outer-input"})
	if err != nil {
		t.Fatal(err)
	}
	final, err := businessWaitWorkflow(ctx, outer)
	if err != nil || final.State != "completed" || string(final.Result) != `{"result":"selected-code-answer"}` {
		t.Fatalf("Workflow business result: state=%s code=%s result=%s err=%v", final.State, final.ErrorCode, final.Result, err)
	}
	inner := businessReceive(t, ctx, finished)
	tr := inner.final.Traces[inner.input.TraceID]
	if len(inner.initial.Traces) != 0 || inner.final.SessionID != innerOptions.SessionID || tr.State != "completed" || !tr.Settled || !tr.ExecutionStopped || len(inner.final.Messages) != 2 || len(inner.final.Invocations) != 0 {
		t.Fatalf("independent Code identity/history/exit mismatch: trace=%+v", tr)
	}
	if string(inner.final.Inputs[inner.input.InputID].Content) != `{"text":"inner-code-prompt-only"}` {
		t.Fatal("business prompt was replaced by outer history")
	}
	businessUsage(t, "outer Workflow", final.Usage, 0, 1)
	businessUsage(t, "inner Code", tr.Usage, 1, 0)
	node := businessWorkflowResult(t, final, "succeeded", "none", true)
	scope := businessReceive(t, ctx, outerScope)
	if scope.WorkflowRunID != outerOptions.RunID || scope.NodeExecutionID != node.ID || scope.SessionID != "" || scope.TraceID != "" || scope.TurnID != "" || scope.InvocationID == tr.InvocationID {
		t.Fatalf("outer Workflow tool scope was replaced by Code scope: %+v", scope)
	}
	if factories.Load() != 1 || tools.Load() != 1 || model.calls.Load() != 1 || outerModel.calls.Load() != 0 || len(final.ModelAttempts) != 0 || len(inner.final.Calls) != 0 || len(inner.final.ModelAttempts) != 1 {
		t.Fatalf("business counts factories/tools/CodeModels/WorkflowModels=%d/%d/%d/%d", factories.Load(), tools.Load(), model.calls.Load(), outerModel.calls.Load())
	}
	for _, attempt := range inner.final.ModelAttempts {
		if attempt.Scope.SessionID != innerOptions.SessionID || attempt.Scope.WorkflowRunID != "" || attempt.Scope.TraceID != inner.input.TraceID || attempt.Scope.InvocationID != tr.InvocationID || attempt.ModelConfigVersion != "business-composition-model-v1" {
			t.Fatalf("independent Code attempt used an outer root or unstable model binding: %+v", attempt)
		}
	}
	for {
		ev := businessReceive(t, ctx, sub.Events)
		if ev.Scope.WorkflowRunID != outerOptions.RunID || ev.Scope.SessionID != "" || ev.Scope.TraceID != "" || ev.Scope.TurnID != "" {
			t.Fatalf("Code event leaked into Workflow subscription: %+v", ev.Scope)
		}
		if ev.DurableSeq != nil && *ev.DurableSeq == final.DurableSeq {
			break
		}
	}
	businessJournals(t, innerOptions, outerOptions)
}

func TestBusinessCompositionOuterRejectionDoesNotCreateInner(t *testing.T) {
	ctx := businessContext(t)
	var factories, innerTools atomic.Int32
	innerModel := &businessModel{final: "must never execute"}
	innerOptions := businessWorkflowOptions(t, "business-deny-inner", businessTool("lookup", func(context.Context, json.RawMessage) (string, error) {
		innerTools.Add(1)
		return "inner-value", nil
	}))
	innerOptions.Models = map[string]einomodel.AgenticModel{"unused": innerModel}
	outerTool := businessTool("run_workflow", func(toolctx context.Context, args json.RawMessage) (output string, err error) {
		factories.Add(1)
		inner, err := sdk.CreateWorkflowAgent(toolctx, innerOptions)
		if err != nil {
			return "", err
		}
		defer func() { err = errors.Join(err, businessClose(inner.Close)) }()
		if _, err = inner.SubmitInput(toolctx, sdk.WorkflowInputCommand{Input: args, Principal: innerOptions.Principal, IdempotencyKey: "inner-input"}); err != nil {
			return "", err
		}
		snap, err := businessWaitWorkflow(toolctx, inner)
		if err != nil {
			return "", err
		}
		if snap.State != "completed" {
			return "", sdk.NewError(sdk.CodeResourceUnavailable, "independent workflow did not complete")
		}
		return string(snap.Result), nil
	})
	outerTool.Execution.RequestedGrantRef = "approve-business-launch-once"
	model := &businessModel{tool: outerTool.Name, callID: "business-deny-provider", arguments: `{"name":"parcel"}`, final: "outer-rejection-observed"}
	outerOptions := businessCodeOptions(t, "business-deny-outer", model, outerTool)
	outer, err := sdk.CreateAgentSession(ctx, outerOptions)
	if err != nil {
		t.Fatal(err)
	}
	businessCleanup(t, outer.Close)
	input, err := outer.SubmitInput(ctx, sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"request business launch"}`), IdempotencyKey: "outer-input"})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := businessWaitCode(ctx, outer, input.TraceID)
	if err != nil || paused.Traces[input.TraceID].State != "paused" {
		t.Fatalf("outer approval did not stop execution: %v", err)
	}
	interaction := onlyConsumerApproval(t, paused)
	if interaction.State != "ready" || interaction.Scope.SessionID != outerOptions.SessionID || interaction.Scope.WorkflowRunID != "" || factories.Load() != 0 || innerTools.Load() != 0 || innerModel.calls.Load() != 0 || model.calls.Load() != 1 {
		t.Fatal("outer approval boundary executed or shared an inner instance")
	}
	businessUsage(t, "unanswered outer", paused.Traces[input.TraceID].Usage, 1, 0)
	answer, err := outer.RespondInteraction(ctx, sdk.InteractionResponse{InteractionID: interaction.ID, Decision: "rejected", ExpectedRevision: paused.Revision, IdempotencyKey: "reject-launch"})
	if err != nil || answer.AcceptedCommit != 0 {
		t.Fatalf("outer rejection: receipt=%+v err=%v", answer, err)
	}
	answered, err := outer.Snapshot(ctx)
	if err != nil || answered.Revision != paused.Revision || answered.Traces[input.TraceID].State != "paused" || !answered.Traces[input.TraceID].ExecutionStopped || factories.Load() != 0 || model.calls.Load() != 1 {
		t.Fatal("responding to outer approval automatically resumed execution")
	}
	if _, err := outer.Resume(ctx, sdk.ResumeCommand{TraceID: input.TraceID, ExpectedRevision: answered.Revision, IdempotencyKey: "resume-rejection"}); err != nil {
		t.Fatal(err)
	}
	final, err := businessWaitCode(ctx, outer, input.TraceID)
	if err != nil || final.Traces[input.TraceID].State != "completed" {
		t.Fatalf("outer did not observe rejected tool: trace=%+v err=%v", final.Traces[input.TraceID], err)
	}
	businessCodeResult(t, final, model.callID, "denied", "none", false)
	businessUsage(t, "rejected outer", final.Traces[input.TraceID].Usage, 2, 0)
	if factories.Load() != 0 || innerTools.Load() != 0 || innerModel.calls.Load() != 0 || model.calls.Load() != 2 || model.paired.Load() != 1 {
		t.Fatalf("rejected business launch counts factories/tools/innerModels/outerModels/paired=%d/%d/%d/%d/%d", factories.Load(), innerTools.Load(), innerModel.calls.Load(), model.calls.Load(), model.paired.Load())
	}
}

func TestBusinessCompositionInnerApprovalRemainsIndependent(t *testing.T) {
	ctx := businessContext(t)
	var factories, effects atomic.Int32
	innerTool := businessTool("lookup", func(context.Context, json.RawMessage) (string, error) {
		effects.Add(1)
		return "inner-approved-value", nil
	})
	innerTool.Execution.RequestedGrantRef = "approve-inner-lookup-once"
	innerOptions := businessWorkflowOptions(t, "business-approval-inner", innerTool)
	innerOptions.Policy = &sdk.ResolvedPolicy{SandboxMode: "read-only", ApprovalPolicy: "ask"}
	innerOptions.Limits.TraceToolCalls = 1
	handle := make(chan *sdk.WorkflowAgent, 1)
	created := make(chan sdk.WorkflowSnapshot, 1)
	pending := make(chan sdk.WorkflowSnapshot, 1)
	outerTool := businessTool("run_workflow", func(toolctx context.Context, args json.RawMessage) (string, error) {
		factories.Add(1)
		inner, err := sdk.CreateWorkflowAgent(toolctx, innerOptions)
		if err != nil {
			return "", err
		}
		// This one test owns this explicit handle for later human-style approval.
		// The closure never copies the outer decision or resumes the inner run.
		handle <- inner
		initial, err := inner.Snapshot(toolctx)
		if err != nil {
			return "", err
		}
		created <- initial
		if _, err := inner.SubmitInput(toolctx, sdk.WorkflowInputCommand{Input: args, Principal: innerOptions.Principal, IdempotencyKey: "inner-input"}); err != nil {
			return "", err
		}
		snap, err := businessWaitWorkflow(toolctx, inner)
		if err != nil {
			return "", err
		}
		pending <- snap
		if snap.State != "paused" || len(snap.Interactions) != 1 || !snap.ExecutionStopped {
			return "", sdk.NewError(sdk.CodeStateConflict, "inner run has no independently stopped approval")
		}
		// A safe pending reference is one business result, not claimed success.
		raw, err := json.Marshal(map[string]string{"workflowRunId": snap.RunID, "state": snap.State})
		return string(raw), err
	})
	outerTool.Execution.RequestedGrantRef = "approve-outer-launch-once"
	model := &businessModel{tool: outerTool.Name, callID: "business-approval-provider", arguments: `{"name":"parcel"}`, final: "outer-business-pending", check: func(result string) error {
		var pending struct {
			RunID string `json:"workflowRunId"`
			State string `json:"state"`
		}
		if json.Unmarshal([]byte(result), &pending) != nil || pending.RunID != innerOptions.RunID || pending.State != "paused" {
			return errors.New("outer result does not explicitly identify the pending independent run")
		}
		return nil
	}}
	outerOptions := businessCodeOptions(t, "business-approval-outer", model, outerTool)
	outerOptions.Policy = &sdk.ResolvedPolicy{SandboxMode: "workspace-write", ApprovalPolicy: "ask"}
	outerOptions.Limits.TraceLogicalModelCalls, outerOptions.Limits.TraceTransportRequests, outerOptions.Limits.TraceToolCalls = 2, 2, 1
	outer, err := sdk.CreateAgentSession(ctx, outerOptions)
	if err != nil {
		t.Fatal(err)
	}
	businessCleanup(t, outer.Close)
	input, err := outer.SubmitInput(ctx, sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"launch independent approval run"}`), IdempotencyKey: "outer-input"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := businessWaitCode(ctx, outer, input.TraceID)
	if err != nil || before.Traces[input.TraceID].State != "paused" {
		t.Fatalf("outer did not wait for its own approval: %v", err)
	}
	outerInteraction := onlyConsumerApproval(t, before)
	answer, err := outer.RespondInteraction(ctx, sdk.InteractionResponse{InteractionID: outerInteraction.ID, Decision: "allowed-once", ExpectedRevision: before.Revision, IdempotencyKey: "approve-launch"})
	if err != nil || answer.AcceptedCommit != 0 || factories.Load() != 0 || effects.Load() != 0 || model.calls.Load() != 1 {
		t.Fatalf("answer ran business closure before explicit Resume: factories=%d effects=%d err=%v", factories.Load(), effects.Load(), err)
	}
	answered, err := outer.Snapshot(ctx)
	if err != nil || answered.Revision != before.Revision || !answered.Traces[input.TraceID].ExecutionStopped {
		t.Fatalf("outer answer changed execution: %v", err)
	}
	if _, err := outer.Resume(ctx, sdk.ResumeCommand{TraceID: input.TraceID, ExpectedRevision: answered.Revision, IdempotencyKey: "resume-launch"}); err != nil {
		t.Fatal(err)
	}
	inner := businessReceive(t, ctx, handle)
	businessCleanup(t, inner.Close)
	initial := businessReceive(t, ctx, created)
	businessUsage(t, "created inner", initial.Usage, 0, 0)
	if initial.State != "created" || len(initial.WorkflowNodes) != 0 {
		t.Fatal("inner factory started an execution")
	}
	final, err := businessWaitCode(ctx, outer, input.TraceID)
	if err != nil || final.Traces[input.TraceID].State != "completed" {
		t.Fatalf("outer pending business result did not complete: trace=%+v err=%v", final.Traces[input.TraceID], err)
	}
	businessCodeResult(t, final, model.callID, "succeeded", "none", true)
	businessUsage(t, "outer pending result", final.Traces[input.TraceID].Usage, 2, 1)
	paused := businessReceive(t, ctx, pending)
	current, err := inner.Snapshot(ctx)
	if err != nil || current.State != "paused" || !current.ExecutionStopped || current.Revision != paused.Revision || current.InstanceID == "" || len(current.Interactions) != 1 || current.CanResume || current.ResumeCode != sdk.CodeStateConflict || effects.Load() != 0 {
		t.Fatalf("outer approval/completion bypassed inner permission: state=%s code=%s effects=%d err=%v", current.State, current.ResumeCode, effects.Load(), err)
	}
	var innerInteraction sdk.WorkflowInteraction
	for _, innerInteraction = range current.Interactions {
	}
	if innerInteraction.ID == outerInteraction.ID || innerInteraction.ToolCallID == outerInteraction.CallID || innerInteraction.InstanceID != current.InstanceID || innerInteraction.State != "ready" || outerInteraction.Scope.SessionID != outerOptions.SessionID || outerInteraction.Scope.WorkflowRunID != "" {
		t.Fatal("business approvals reused an interaction, call or root identity")
	}
	businessUsage(t, "unanswered inner", current.Usage, 0, 0)
	innerAnswer, err := inner.RespondInteraction(ctx, sdk.WorkflowInteractionResponse{InteractionID: innerInteraction.ID, Decision: "allowed-once", ExpectedRevision: current.Revision, IdempotencyKey: "approve-inner", Principal: innerOptions.Principal})
	if err != nil || innerAnswer.AcceptedCommit != 0 || innerAnswer.ReceiptScope != "instance" || innerAnswer.InstanceID != current.InstanceID || effects.Load() != 0 {
		t.Fatalf("inner answer automatically executed or lost its instance: receipt=%+v effects=%d err=%v", innerAnswer, effects.Load(), err)
	}
	innerAnswered, err := inner.Snapshot(ctx)
	if err != nil || innerAnswered.State != "paused" || innerAnswered.Revision != current.Revision || !innerAnswered.ExecutionStopped {
		t.Fatalf("inner answer automatically resumed: %v", err)
	}
	if _, err := inner.Resume(ctx, sdk.WorkflowControlCommand{IdempotencyKey: "resume-inner", ExpectedRevision: &innerAnswered.Revision, Principal: innerOptions.Principal}); err != nil {
		t.Fatal(err)
	}
	innerFinal, err := businessWaitWorkflow(ctx, inner)
	if err != nil || innerFinal.State != "completed" || string(innerFinal.Result) != `{"result":"inner-approved-value"}` || innerFinal.InstanceID != current.InstanceID {
		t.Fatalf("explicit inner approval/Resume did not finish: state=%s err=%v", innerFinal.State, err)
	}
	businessWorkflowResult(t, innerFinal, "succeeded", "none", true)
	businessUsage(t, "approved inner", innerFinal.Usage, 0, 1)
	stillOuter, err := outer.Snapshot(ctx)
	if err != nil || stillOuter.Revision != final.Revision || effects.Load() != 1 || factories.Load() != 1 || model.calls.Load() != 2 || model.paired.Load() != 1 {
		t.Fatalf("inner execution changed outer accounting/history: effects=%d factories=%d models=%d paired=%d err=%v", effects.Load(), factories.Load(), model.calls.Load(), model.paired.Load(), err)
	}
	businessUsage(t, "unchanged outer", stillOuter.Traces[input.TraceID].Usage, 2, 1)
	businessJournals(t, outerOptions, innerOptions)
}

func TestBusinessCompositionInnerBudgetFailureIsNotSuccess(t *testing.T) {
	ctx := businessContext(t)
	var factories, tools atomic.Int32
	innerOptions := businessWorkflowOptions(t, "business-budget-inner", businessTool("lookup", func(context.Context, json.RawMessage) (string, error) {
		tools.Add(1)
		return "first-read-only", nil
	}))
	innerOptions.Limits.TraceToolCalls = 1
	innerOptions.Definition.Nodes = []sdk.WorkflowNode{{ID: "s", Type: "start"},
		{ID: "t", Type: "tool", Tool: "lookup", Inputs: map[string]sdk.WorkflowValue{"name": {Ref: &sdk.WorkflowRef{Node: "s", Field: "name"}}}},
		{ID: "t2", Type: "tool", Tool: "lookup", Inputs: map[string]sdk.WorkflowValue{"name": {Ref: &sdk.WorkflowRef{Node: "s", Field: "name"}}}},
		{ID: "e", Type: "end", Inputs: map[string]sdk.WorkflowValue{"result": {Ref: &sdk.WorkflowRef{Node: "t2", Field: "result"}}}}}
	innerOptions.Definition.Edges = []sdk.WorkflowEdge{{From: "s", To: "t"}, {From: "t", To: "t2"}, {From: "t2", To: "e"}}
	innerModel := &businessModel{final: "must never execute"}
	innerOptions.Models = map[string]einomodel.AgenticModel{"unused": innerModel}
	finished := make(chan sdk.WorkflowSnapshot, 1)
	outerTool := businessTool("run_workflow", func(toolctx context.Context, args json.RawMessage) (output string, err error) {
		factories.Add(1)
		inner, err := sdk.CreateWorkflowAgent(toolctx, innerOptions)
		if err != nil {
			return "", err
		}
		defer func() { err = errors.Join(err, businessClose(inner.Close)) }()
		if _, err := inner.SubmitInput(toolctx, sdk.WorkflowInputCommand{Input: args, Principal: innerOptions.Principal, IdempotencyKey: "inner-input"}); err != nil {
			return "", err
		}
		snap, err := businessWaitWorkflow(toolctx, inner)
		if err != nil {
			return "", err
		}
		finished <- snap
		if snap.State != "completed" {
			// Return the actual failure. The controlled outer executor owns the
			// conservative failed/unknown observation; the closure cannot clear it.
			return "", sdk.NewError(snap.ErrorCode, "independent workflow failed under its own budget")
		}
		return string(snap.Result), nil
	})
	model := &businessModel{tool: outerTool.Name, callID: "business-budget-provider", arguments: `{"name":"parcel"}`, final: "must not hide failed business execution"}
	outerOptions := businessCodeOptions(t, "business-budget-outer", model, outerTool)
	outerOptions.Limits.TraceLogicalModelCalls, outerOptions.Limits.TraceTransportRequests, outerOptions.Limits.TraceToolCalls = 2, 2, 1
	outer, err := sdk.CreateAgentSession(ctx, outerOptions)
	if err != nil {
		t.Fatal(err)
	}
	businessCleanup(t, outer.Close)
	input, err := outer.SubmitInput(ctx, sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run independent limited workflow"}`), IdempotencyKey: "outer-input"})
	if err != nil {
		t.Fatal(err)
	}
	final, err := businessWaitCode(ctx, outer, input.TraceID)
	if err != nil || final.Traces[input.TraceID].State != "failed" || !final.Traces[input.TraceID].Settled {
		t.Fatalf("failed/unknown business result was turned into success: trace=%+v err=%v", final.Traces[input.TraceID], err)
	}
	inner := businessReceive(t, ctx, finished)
	if inner.State != "failed" || inner.ErrorCode != sdk.CodeBudgetExhausted || !inner.ExecutionStopped || inner.RunID != innerOptions.RunID || len(inner.WorkflowNodes) != 2 || inner.FailedNode == "" || inner.WorkflowNodes[inner.FailedNode].NodeID != "t2" || inner.WorkflowNodes[inner.FailedNode].ErrorCode != sdk.CodeBudgetExhausted {
		t.Fatalf("inner budget did not stop the second real node: state=%s code=%s failedNode=%s", inner.State, inner.ErrorCode, inner.FailedNode)
	}
	businessUsage(t, "limited inner", inner.Usage, 0, 1)
	businessUsage(t, "independent outer", final.Traces[input.TraceID].Usage, 1, 1)
	call := businessCodeResult(t, final, model.callID, "failed", "unknown", true)
	if len(final.PendingReconciliations) != 1 || final.PendingReconciliations[0].CallID != call.Call.CallID || final.Resume[input.TraceID].CanResume || final.Resume[input.TraceID].Code != sdk.CodeIncompatibleResume {
		t.Fatal("outer lost its actual failed/unknown business observation")
	}
	if factories.Load() != 1 || tools.Load() != 1 || model.calls.Load() != 1 || innerModel.calls.Load() != 0 || len(inner.ModelAttempts) != 0 {
		t.Fatalf("budget counts factories/innerTools/outerModels/innerModels=%d/%d/%d/%d", factories.Load(), tools.Load(), model.calls.Load(), innerModel.calls.Load())
	}
	businessJournals(t, outerOptions, innerOptions)
}

func TestBusinessCompositionLinkedCancellationWaitsForActualExit(t *testing.T) {
	ctx := businessContext(t)
	release, unrelatedRelease := make(chan struct{}), make(chan struct{})
	var releaseOnce, unrelatedOnce sync.Once
	// Failed assertions also release every non-cooperative callback before
	// cleanup waits for an independently owned execution or writer.
	defer func() {
		releaseOnce.Do(func() { close(release) })
		unrelatedOnce.Do(func() { close(unrelatedRelease) })
	}()

	unrelatedModel := &businessModel{final: "unrelated-code-answer", entered: make(chan struct{}), release: unrelatedRelease, cancelled: make(chan struct{})}
	unrelatedCodeOptions := businessCodeOptions(t, "business-cancel-unrelated-code", unrelatedModel)
	unrelatedCode, err := sdk.CreateAgentSession(ctx, unrelatedCodeOptions)
	if err != nil {
		t.Fatal(err)
	}
	businessCleanup(t, unrelatedCode.Close)
	unrelatedInput, err := unrelatedCode.SubmitInput(ctx, sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"unrelated running Code instance"}`), IdempotencyKey: "unrelated-input"})
	if err != nil {
		t.Fatal(err)
	}
	businessReceive(t, ctx, unrelatedModel.entered)
	unrelatedEntered, unrelatedCancelled := make(chan struct{}), make(chan struct{})
	var unrelatedTools atomic.Int32
	unrelatedWorkflowOptions := businessWorkflowOptions(t, "business-cancel-unrelated-workflow", businessTool("hold_unrelated", func(ctx context.Context, _ json.RawMessage) (string, error) {
		unrelatedTools.Add(1)
		close(unrelatedEntered)
		select {
		case <-unrelatedRelease:
			return "unrelated-workflow-answer", nil
		case <-ctx.Done():
			close(unrelatedCancelled)
			<-unrelatedRelease
			return "", ctx.Err()
		}
	}))
	unrelatedWorkflow, err := sdk.CreateWorkflowAgent(ctx, unrelatedWorkflowOptions)
	if err != nil {
		t.Fatal(err)
	}
	businessCleanup(t, unrelatedWorkflow.Close)
	if _, err := unrelatedWorkflow.SubmitInput(ctx, sdk.WorkflowInputCommand{Input: json.RawMessage(`{"name":"unrelated"}`), Principal: unrelatedWorkflowOptions.Principal, IdempotencyKey: "unrelated-input"}); err != nil {
		t.Fatal(err)
	}
	businessReceive(t, ctx, unrelatedEntered)
	codeBefore, err := unrelatedCode.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	workflowBefore, err := unrelatedWorkflow.Snapshot(ctx)
	if err != nil || codeBefore.Traces[unrelatedInput.TraceID].State != "running" || codeBefore.Traces[unrelatedInput.TraceID].ExecutionStopped || workflowBefore.State != "running" || workflowBefore.ExecutionStopped {
		t.Fatalf("unrelated runs are not actually running: err=%v", err)
	}

	entered, cancellationObserved := make(chan struct{}), make(chan struct{})
	var factories, tools atomic.Int32
	innerOptions := businessWorkflowOptions(t, "business-cancel-inner", businessTool("hold_inner", func(ctx context.Context, _ json.RawMessage) (string, error) {
		tools.Add(1)
		close(entered)
		<-ctx.Done()
		close(cancellationObserved)
		// Receiving cancellation is not exit: wait for the explicit release.
		<-release
		return "", ctx.Err()
	}))
	innerOptions.Limits.TraceToolCalls = 1
	handle := make(chan *sdk.WorkflowAgent, 1)
	created := make(chan sdk.WorkflowSnapshot, 1)
	type proof struct {
		final          sdk.WorkflowSnapshot
		receipt, again sdk.WorkflowOperationReceipt
		afterRevision  uint64
	}
	finished := make(chan proof, 1)
	closed := make(chan error, 1)
	outerTool := businessTool("run_workflow", func(toolctx context.Context, args json.RawMessage) (output string, err error) {
		factories.Add(1)
		inner, err := sdk.CreateWorkflowAgent(toolctx, innerOptions)
		if err != nil {
			return "", err
		}
		// Only this handle is linked. The closure never captures the unrelated
		// Code or Workflow instances above.
		handle <- inner
		defer func() {
			closeErr := businessClose(inner.Close)
			err = errors.Join(err, closeErr)
			closed <- closeErr
		}()
		initial, err := inner.Snapshot(toolctx)
		if err != nil {
			return "", err
		}
		created <- initial
		if _, err := inner.SubmitInput(toolctx, sdk.WorkflowInputCommand{Input: args, Principal: innerOptions.Principal, IdempotencyKey: "linked-input"}); err != nil {
			return "", err
		}
		<-toolctx.Done()
		// Use the linked run's own trusted principal and private key, with a
		// bounded context that is independent of the cancelled outer tool.
		controlctx, cancel := context.WithTimeout(context.WithoutCancel(toolctx), 5*time.Second)
		defer cancel()
		command := sdk.WorkflowControlCommand{IdempotencyKey: "cancel-linked-inner", Principal: innerOptions.Principal, Reason: "outer business tool was cancelled"}
		receipt, err := inner.Cancel(controlctx, command)
		if err != nil {
			return "", err
		}
		final, err := businessWaitWorkflow(controlctx, inner)
		if err != nil {
			return "", err
		}
		if final.State != "cancelled" || !final.ExecutionStopped {
			return "", sdk.NewError(sdk.CodeStateConflict, "linked inner has not actually exited")
		}
		again, err := inner.Cancel(controlctx, command)
		if err != nil {
			return "", err
		}
		after, err := inner.Snapshot(controlctx)
		if err != nil {
			return "", err
		}
		finished <- proof{final, receipt, again, after.Revision}
		return "", toolctx.Err()
	})
	model := &businessModel{tool: outerTool.Name, callID: "business-cancel-provider", arguments: `{"name":"linked"}`, final: "must not run after cancellation"}
	outerOptions := businessCodeOptions(t, "business-cancel-outer", model, outerTool)
	outerOptions.Limits.TraceToolCalls = 1
	outer, err := sdk.CreateAgentSession(ctx, outerOptions)
	if err != nil {
		t.Fatal(err)
	}
	businessCleanup(t, outer.Close)
	input, err := outer.SubmitInput(ctx, sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run linked business operation"}`), IdempotencyKey: "linked-input"})
	if err != nil {
		t.Fatal(err)
	}
	inner := businessReceive(t, ctx, handle)
	initial := businessReceive(t, ctx, created)
	if initial.State != "created" || !initial.ExecutionStopped || len(initial.WorkflowNodes) != 0 {
		t.Fatal("linked inner factory executed work")
	}
	businessUsage(t, "created linked inner", initial.Usage, 0, 0)
	businessReceive(t, ctx, entered)
	// Observe durable admission first: 20ms is a caller wait deadline, not an
	// admission SLA, and cannot turn a still-running callback into exit proof.
	cancelCommand := sdk.CancelTraceRequest{TraceID: input.TraceID, IdempotencyKey: "cancel-business-outer"}
	receipt, err := outer.CancelTrace(ctx, cancelCommand)
	if err != nil || receipt.OperationID == "" || receipt.AcceptedCommit == 0 {
		t.Fatalf("outer cancellation intent was not accepted: receipt=%+v err=%v", receipt, err)
	}
	waitctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	err = outer.Cancel(waitctx, input.TraceID)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("20ms caller wait unexpectedly proved exit: %v", err)
	}
	businessReceive(t, ctx, cancellationObserved)
	outerCancelling, err := outer.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	innerCancelling, err := inner.Snapshot(ctx)
	tr := outerCancelling.Traces[input.TraceID]
	if err != nil || tr.State != "cancelling" || tr.Settled || tr.ExecutionStopped || outerCancelling.Operations[receipt.OperationID].State != "accepted" || innerCancelling.State != "cancelling" || innerCancelling.ExecutionStopped {
		t.Fatalf("cancellation settled before the runner exited: outer=%+v innerState=%s innerStopped=%v err=%v", tr, innerCancelling.State, innerCancelling.ExecutionStopped, err)
	}
	select {
	case <-finished:
		t.Fatal("business closure returned before the inner actually exited")
	default:
	}
	select {
	case <-closed:
		t.Fatal("inner writer closed before its runner actually exited")
	default:
	}
	businessUsage(t, "cancelling outer", tr.Usage, 1, 1)
	businessUsage(t, "cancelling inner", innerCancelling.Usage, 0, 1)
	releaseOnce.Do(func() { close(release) })
	innerFinal := businessReceive(t, ctx, finished)
	if closeErr := businessReceive(t, ctx, closed); closeErr != nil {
		t.Fatalf("linked inner Close after actual exit: %v", closeErr)
	}
	outerFinal, err := businessWaitCode(ctx, outer, input.TraceID)
	if err != nil || outerFinal.Traces[input.TraceID].State != "cancelled" || !outerFinal.Traces[input.TraceID].Settled || !outerFinal.Traces[input.TraceID].ExecutionStopped {
		t.Fatalf("outer did not actually exit after release: trace=%+v err=%v", outerFinal.Traces[input.TraceID], err)
	}
	if innerFinal.final.RunID != innerOptions.RunID || innerFinal.final.State != "cancelled" || !innerFinal.final.ExecutionStopped || innerFinal.receipt.OperationID == "" || innerFinal.receipt != innerFinal.again || innerFinal.afterRevision != innerFinal.final.Revision || innerFinal.final.Operations[innerFinal.receipt.OperationID].State != "completed" {
		t.Fatal("inner cancellation did not provide stable, actual exit evidence")
	}
	businessCodeResult(t, outerFinal, model.callID, "failed", "unknown", true)
	businessWorkflowResult(t, innerFinal.final, "failed", "unknown", true)
	businessUsage(t, "cancelled outer", outerFinal.Traces[input.TraceID].Usage, 1, 1)
	businessUsage(t, "cancelled inner", innerFinal.final.Usage, 0, 1)
	if factories.Load() != 1 || tools.Load() != 1 || model.calls.Load() != 1 || innerFinal.final.InstanceID != initial.InstanceID {
		t.Fatalf("linked cancellation duplicated execution: factories=%d tools=%d models=%d", factories.Load(), tools.Load(), model.calls.Load())
	}
	// The durable cancel receipt also needs a completed operation after exit.
	status, err := outer.GetOperation(ctx, receipt.OperationID)
	if err != nil || status.State != "completed" || status.ResultRef != "cancelled" {
		t.Fatalf("outer cancel operation did not complete after exit: %+v err=%v", status, err)
	}
	if again, err := outer.CancelTrace(ctx, cancelCommand); err != nil || again != receipt {
		t.Fatalf("outer cancellation replay changed identity: %+v err=%v", again, err)
	}
	// Observe a real renewal, even when the linked cancellation finishes within
	// the first lease. This covers positive revision growth without changing
	// production timers, stopping the unrelated runner, or avoiding a window.
	renewctx, stopRenewalWait := context.WithTimeout(ctx, 5*time.Second)
	defer stopRenewalWait()
	renewTick := time.NewTicker(time.Millisecond)
	defer renewTick.Stop()
	var codeAfter sdk.Snapshot
	for {
		codeAfter, err = unrelatedCode.Snapshot(renewctx)
		if err != nil {
			t.Fatalf("unrelated Code renewal observation: %v", err)
		}
		if codeAfter.Traces[unrelatedInput.TraceID].Activity.Revision > codeBefore.Traces[unrelatedInput.TraceID].Activity.Revision {
			break
		}
		select {
		case <-renewctx.Done():
			t.Fatalf("unrelated Code never renewed its own running activity: %v", renewctx.Err())
		case <-renewTick.C:
		}
	}
	workflowAfter, err := unrelatedWorkflow.Snapshot(ctx)
	t.Logf("unrelated Code before/after: revision=%d/%d session=%s/%s activeTrace=%s/%s cursor=%d/%d trace=%+v/%+v operations=%+v/%+v", codeBefore.Revision, codeAfter.Revision, codeBefore.SessionID, codeAfter.SessionID, codeBefore.ActiveTrace, codeAfter.ActiveTrace, codeBefore.Cursor, codeAfter.Cursor, codeBefore.Traces[unrelatedInput.TraceID], codeAfter.Traces[unrelatedInput.TraceID], codeBefore.Operations, codeAfter.Operations)
	t.Logf("unrelated Workflow before/after: revision=%d/%d state=%s/%s stopped=%v/%v instance=%s/%s run=%s/%s durableSeq=%d/%d cursor=%s/%s usage=%+v/%+v nodes=%+v/%+v operations=%+v/%+v err=%v", workflowBefore.Revision, workflowAfter.Revision, workflowBefore.State, workflowAfter.State, workflowBefore.ExecutionStopped, workflowAfter.ExecutionStopped, workflowBefore.InstanceID, workflowAfter.InstanceID, workflowBefore.RunID, workflowAfter.RunID, workflowBefore.DurableSeq, workflowAfter.DurableSeq, workflowBefore.Cursor, workflowAfter.Cursor, workflowBefore.Usage, workflowAfter.Usage, workflowBefore.WorkflowNodes, workflowAfter.WorkflowNodes, workflowBefore.Operations, workflowAfter.Operations, err)
	for _, pair := range []struct {
		label         string
		before, after any
	}{{"Code", codeBefore, codeAfter}, {"Workflow", workflowBefore, workflowAfter}} {
		before, after := reflect.ValueOf(pair.before), reflect.ValueOf(pair.after)
		for i := 0; i < before.NumField(); i++ {
			if !reflect.DeepEqual(before.Field(i).Interface(), after.Field(i).Interface()) {
				t.Logf("unrelated %s changed field %s: before=%+v after=%+v", pair.label, before.Type().Field(i).Name, before.Field(i).Interface(), after.Field(i).Interface())
			}
		}
	}
	// Every new durable fact must be this same trace's own activity renewal,
	// not a control operation, event, message, call, budget claim or new scope.
	previousTrace := *codeBefore.Traces[unrelatedInput.TraceID]
	previousRevision := codeBefore.Revision
	for _, commit := range businessJournalThrough(t, filepath.Join(unrelatedCodeOptions.StateRoot, "sessions", unrelatedCodeOptions.SessionID, "journal.jsonl"), codeAfter.Revision) {
		if commit.CommitSeq <= codeBefore.Revision {
			continue
		}
		if commit.RecordType != "commit" || commit.Version != 1 || commit.CommitID == "" || commit.CommitSeq != previousRevision+1 || commit.ExpectedPreviousSeq != previousRevision || len(commit.ControlRecords) != 1 || len(commit.Entries) != 0 || len(commit.Events) != 0 || len(commit.BranchUpdates) != 0 || commit.IdempotencyKey != "" || len(commit.ContentDigest) != 0 {
			t.Fatalf("unrelated Code added more than its own activity renewal: %+v", commit)
		}
		record := commit.ControlRecords[0]
		var next sdk.TraceState
		if record.Type != "trace" || record.Version != 1 || record.ID != unrelatedInput.TraceID || record.ParentID != "" || json.Unmarshal(record.Payload, &next) != nil {
			t.Fatalf("unrelated Code renewal changed record owner/type: %+v", record)
		}
		oldActivity, activity := previousTrace.Activity, next.Activity
		if !activity.Known || activity.Unknown || activity.ExecutionID == "" || activity.ExecutionID != oldActivity.ExecutionID || activity.Revision != oldActivity.Revision+1 || activity.Uncertain != oldActivity.Uncertain || activity.Settled <= oldActivity.Settled || activity.Settled-oldActivity.Settled > oldActivity.Reserved || activity.Reserved <= 0 || activity.Reserved != min(time.Second, next.Limits.ActivityBudget-activity.Settled-activity.Uncertain) {
			t.Fatalf("unrelated Code durable change is not a valid own renewal: before=%+v after=%+v", oldActivity, activity)
		}
		normalized := next
		normalized.Activity = oldActivity
		if !reflect.DeepEqual(normalized, previousTrace) {
			t.Fatalf("unrelated Code renewal changed non-activity trace facts: before=%+v after=%+v", previousTrace, next)
		}
		t.Logf("verified own Code activity renewal: commit=%d trace=%s execution=%s activity=%+v -> %+v; entries/events/branches/operations=0", commit.CommitSeq, record.ID, activity.ExecutionID, oldActivity, activity)
		previousTrace, previousRevision = next, commit.CommitSeq
	}
	if previousRevision != codeAfter.Revision || !reflect.DeepEqual(previousTrace, *codeAfter.Traces[unrelatedInput.TraceID]) || codeAfter.Revision-codeBefore.Revision != previousTrace.Activity.Revision-codeBefore.Traces[unrelatedInput.TraceID].Activity.Revision {
		t.Fatal("unrelated Code revision growth is not fully explained by its own durable renewals")
	}
	// Compare every remaining public field, including model attempt identity,
	// full usage, limits, state, exit proof, history and control operations.
	normalizedCode := codeAfter
	normalizedCode.Revision = codeBefore.Revision
	normalizedCode.Traces = maps.Clone(codeAfter.Traces)
	normalizedTrace := *codeAfter.Traces[unrelatedInput.TraceID]
	normalizedTrace.Activity = codeBefore.Traces[unrelatedInput.TraceID].Activity
	normalizedCode.Traces[unrelatedInput.TraceID] = &normalizedTrace
	if err != nil || codeAfter.Revision < codeBefore.Revision || codeAfter.Traces[unrelatedInput.TraceID].State != "running" || codeAfter.Traces[unrelatedInput.TraceID].ExecutionStopped || workflowAfter.Revision != workflowBefore.Revision || workflowAfter.State != "running" || workflowAfter.ExecutionStopped || workflowAfter.InstanceID != workflowBefore.InstanceID || !reflect.DeepEqual(normalizedCode, codeBefore) || !reflect.DeepEqual(workflowAfter, workflowBefore) {
		t.Fatalf("linked cancellation affected unrelated running instances beyond own Code activity: err=%v", err)
	}
	if unrelatedModel.calls.Load() != 1 || unrelatedTools.Load() != 1 || len(codeAfter.Operations) != 0 || len(workflowAfter.Operations) != 0 {
		t.Fatalf("unrelated runs received extra calls/control operations: models=%d tools=%d codeOperations=%+v workflowOperations=%+v", unrelatedModel.calls.Load(), unrelatedTools.Load(), codeAfter.Operations, workflowAfter.Operations)
	}
	select {
	case <-unrelatedModel.cancelled:
		t.Fatal("unrelated Code model received cancellation")
	case <-unrelatedCancelled:
		t.Fatal("unrelated Workflow tool received cancellation")
	default:
	}
	businessUsage(t, "unrelated Code unchanged", codeAfter.Traces[unrelatedInput.TraceID].Usage, 1, 0)
	businessUsage(t, "unrelated Workflow unchanged", workflowAfter.Usage, 0, 1)
	unrelatedOnce.Do(func() { close(unrelatedRelease) })
	codeDone, err := businessWaitCode(ctx, unrelatedCode, unrelatedInput.TraceID)
	if err != nil || codeDone.Traces[unrelatedInput.TraceID].State != "completed" || unrelatedModel.calls.Load() != 1 {
		t.Fatalf("unrelated Code could not complete naturally: %v", err)
	}
	workflowDone, err := businessWaitWorkflow(ctx, unrelatedWorkflow)
	if err != nil || workflowDone.State != "completed" || unrelatedTools.Load() != 1 {
		t.Fatalf("unrelated Workflow could not complete naturally: %v", err)
	}
	businessJournals(t, outerOptions, innerOptions)
}
