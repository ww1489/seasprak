package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

// stopChild uses the actual Close/reopen path, not an injected paused View.
func stopChild(t *testing.T, extra []schema.FunctionToolCall, defs []tools.Definition) (Options, agent.InputReceipt) {
	t.Helper()
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	main := testkit.NewFake(toolCalls(append([]schema.FunctionToolCall{{CallID: "delegate", Name: delegateToolName, Arguments: delegateArgs("worker", "work")}}, extra...)...), testkit.Step{Text: "never"})
	child := testkit.NewFake(testkit.Step{Gate: gate, Text: "late"})
	var childTools []string
	for _, def := range defs {
		childTools = append(childTools, def.Name)
	}
	opts := subagentOptions(agentRoots(t), "child-recovery", main, []agent.AgentDefinition{{Name: "worker", Version: "v1", Instruction: "W", Model: versionedPauseModel{child}, Delegable: true, Tools: childTools}}, defs)
	opts.Model = versionedPauseModel{main}
	s := openSubagentSession(t, opts, true)
	in := submitPrompt(t, s, "delegate")
	waitFor(t, func() bool { return child.Calls() == 1 })
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	return opts, in
}

func TestSafeInterruptedDelegationCloseDoesNotPersistInternalSignal(t *testing.T) {
	opts, in := stopChild(t, nil, nil)
	s := openSubagentSession(t, opts, false)
	if tr := s.rt.manager.View().Traces[in.TraceID]; tr.Error != "" {
		t.Fatalf("safe Close recorded an internal failure: %q", tr.Error)
	}
}

func TestInterruptedChildCancelFinalizesOriginalCall(t *testing.T) {
	opts, in := stopChild(t, nil, nil)
	main, child := testkit.NewFake(), testkit.NewFake()
	opts.Model, opts.Agents[0].Model = versionedPauseModel{main}, versionedPauseModel{child}
	s := openSubagentSession(t, opts, false)
	inv := onlyInvocation(t, s.rt.manager.View())
	if err := s.Cancel(t.Context(), in.TraceID); err != nil {
		t.Fatal(err)
	}
	v := s.rt.manager.View()
	parent := v.Calls[inv.ParentCallID]
	if tr := v.Traces[in.TraceID]; tr.State != "cancelled" || !tr.Settled || v.HasUnresolvedEffects() {
		t.Fatalf("cancel trace=%+v unresolved=%v", tr, v.HasUnresolvedEffects())
	}
	if parent.Observation == nil || parent.Observation.SideEffect != "none" || parent.Observation.Executed || !v.Turns[parent.Scope.TurnID].Ended || v.Invocations[inv.ID].State != "cancelled" {
		t.Fatalf("cancel invocation=%+v parent=%+v", v.Invocations[inv.ID], parent)
	}
	if main.Calls() != 0 || child.Calls() != 0 {
		t.Fatal("cancel ran a model")
	}
}

func TestInterruptedChildResumeRejectsChangedBuild(t *testing.T) {
	opts, in := stopChild(t, nil, nil)
	main, child := testkit.NewFake(testkit.Step{Text: "never"}), testkit.NewFake(testkit.Step{Text: "never"})
	opts.Model, opts.Agents[0].Model = versionedPauseModel{main}, versionedPauseModel{child}
	opts.Instruction = "different-parent-instruction"
	s := openSubagentSession(t, opts, false)
	v := s.rt.manager.View()
	snap, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Resume[in.TraceID].CanResume {
		t.Fatal("changed build incorrectly advertised as resumable")
	}
	_, err = s.Resume(t.Context(), ResumeCommand{TraceID: in.TraceID, ExpectedRevision: v.LastSeq})
	requireCode(t, err, product.CodeIncompatibleResume)
	if s.rt.manager.View().LastSeq != v.LastSeq || main.Calls() != 0 || child.Calls() != 0 {
		t.Fatal("incompatible resume wrote or ran")
	}
}

func TestInterruptedChildResumeKeepsSiblingToolResults(t *testing.T) {
	var probes atomic.Int32
	opts, in := stopChild(t, []schema.FunctionToolCall{{CallID: "probe", Name: "probe", Arguments: `{}`}}, []tools.Definition{countedTool("probe", &probes, nil)})
	main, child := testkit.NewFake(testkit.Step{Text: "parent done"}), testkit.NewFake(testkit.Step{Text: "child done"})
	opts.Model, opts.Agents[0].Model = versionedPauseModel{main}, versionedPauseModel{child}
	s := openSubagentSession(t, opts, false)
	before := s.rt.manager.View()
	_, err := s.Resume(t.Context(), ResumeCommand{TraceID: in.TraceID, ExpectedRevision: before.LastSeq})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	v := s.rt.manager.View()
	var resultIDs []string
	for _, msg := range v.Messages {
		if msg.Kind == agent.KindToolResult {
			resultIDs = append(resultIDs, msg.Scope.ToolCallID)
		}
	}
	parent := delegateRecord(t, s)
	turn := v.Turns[parent.Scope.TurnID]
	if len(resultIDs) != len(turn.CallIDs) {
		t.Fatalf("tool group lost sibling: results=%v calls=%v", resultIDs, turn.CallIDs)
	}
	for i, id := range turn.CallIDs {
		if resultIDs[i] != id {
			t.Fatalf("tool group order=%v want=%v", resultIDs, turn.CallIDs)
		}
	}
	if probes.Load() > 1 || main.Calls() != 1 || child.Calls() != 1 || v.Traces[in.TraceID].State != "completed" {
		t.Fatalf("unexpected replay probes=%d main=%d child=%d trace=%+v", probes.Load(), main.Calls(), child.Calls(), v.Traces[in.TraceID])
	}
}

func TestInterruptedChildResumeCloseRetainsInvocationProgress(t *testing.T) {
	opts, in := stopChild(t, nil, nil)
	gate := make(chan struct{})
	defer close(gate)
	main, child := testkit.NewFake(), testkit.NewFake(testkit.Step{Gate: gate, Text: "late again"})
	opts.Model, opts.Agents[0].Model = versionedPauseModel{main}, versionedPauseModel{child}
	s := openSubagentSession(t, opts, false)
	v := s.rt.manager.View()
	original := onlyInvocation(t, v)
	_, err := s.Resume(t.Context(), ResumeCommand{TraceID: in.TraceID, ExpectedRevision: v.LastSeq})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return child.Calls() == 1 })
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	v = s.rt.manager.View()
	if inv := v.Invocations[original.ID]; inv.State != "interrupted" || inv.ModelCalls != original.ModelCalls+1 {
		t.Fatalf("resume Close lost child progress: %+v original=%+v", inv, original)
	}
	if p := v.Calls[original.ParentCallID]; p.Observation != nil {
		t.Fatalf("safe interrupted child was finalized: %+v", p.Observation)
	}
}

type childContinuationModel struct {
	versionedPauseModel
	request atomic.Value
}

func (m *childContinuationModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	raw, _ := json.Marshal(input)
	m.request.Store(string(raw))
	return m.FakeModel.Generate(ctx, input, opts...)
}

func (m *childContinuationModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	raw, _ := json.Marshal(input)
	m.request.Store(string(raw))
	return m.FakeModel.Stream(ctx, input, opts...)
}

func TestInterruptedChildParentReceivesOriginalInputAndResultOnce(t *testing.T) {
	opts, in := stopChild(t, nil, nil)
	main := &childContinuationModel{versionedPauseModel: versionedPauseModel{testkit.NewFake(testkit.Step{Text: "parent done"})}}
	child := testkit.NewFake(testkit.Step{Text: "child recovered"})
	opts.Model, opts.Agents[0].Model = main, versionedPauseModel{child}
	s := openSubagentSession(t, opts, false)
	before := s.rt.manager.View()
	cmd := ResumeCommand{TraceID: in.TraceID, ExpectedRevision: before.LastSeq, IdempotencyKey: "child-parent-request"}
	first, err := s.Resume(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	second, err := s.Resume(t.Context(), cmd)
	if err != nil || first != second {
		t.Fatalf("duplicate receipt=%+v err=%v", second, err)
	}
	var request []*schema.AgenticMessage
	if raw, ok := main.request.Load().(string); !ok || json.Unmarshal([]byte(raw), &request) != nil {
		t.Fatal("parent received no captured request")
	}
	userInputs, results := 0, 0
	for _, msg := range request {
		for _, block := range msg.ContentBlocks {
			if block == nil {
				continue
			}
			if block.UserInputText != nil && block.UserInputText.Text == "delegate" {
				userInputs++
			}
			if result := block.FunctionToolResult; result != nil && result.CallID == "delegate" {
				results++
				if len(result.Content) != 1 || result.Content[0].Text == nil || !strings.Contains(result.Content[0].Text.Text, "child recovered") {
					t.Fatalf("parent received wrong result: %+v", result)
				}
			}
		}
	}
	if userInputs != 1 || results != 1 || main.Calls() != 1 || child.Calls() != 1 {
		t.Fatalf("parent input/result duplicated or missing: inputs=%d results=%d main=%d child=%d", userInputs, results, main.Calls(), child.Calls())
	}
}

func TestInterruptedChildResumeImmediateCancelLeavesNoRunningChild(t *testing.T) {
	for i := 0; i < 10; i++ {
		opts, in := stopChild(t, nil, nil)
		gate := make(chan struct{})
		child, main := testkit.NewFake(testkit.Step{Gate: gate, Text: "late"}), testkit.NewFake()
		opts.Model, opts.Agents[0].Model = versionedPauseModel{main}, versionedPauseModel{child}
		s := openSubagentSession(t, opts, false)
		before := s.rt.manager.View()
		inv := onlyInvocation(t, before)
		if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: in.TraceID, ExpectedRevision: before.LastSeq}); err != nil {
			close(gate)
			t.Fatal(err)
		}
		if err := s.Cancel(t.Context(), in.TraceID); err != nil {
			close(gate)
			t.Fatal(err)
		}
		close(gate)
		v := s.rt.manager.View()
		if v.Invocations[inv.ID].State != "cancelled" || v.HasUnresolvedEffects() || v.Calls[inv.ParentCallID].Observation == nil || main.Calls() != 0 {
			t.Fatalf("immediate cancellation lost child: inv=%+v call=%+v main=%d", v.Invocations[inv.ID], v.Calls[inv.ParentCallID], main.Calls())
		}
	}
}

func TestInterruptedChildResumeCancelAfterKnownToolRetainsKnownOutcome(t *testing.T) {
	var effects atomic.Int32
	opts, in := stopChild(t, nil, []tools.Definition{countedTool("probe", &effects, nil)})
	gate := make(chan struct{})
	defer close(gate)
	child := testkit.NewFake(toolCalls(schema.FunctionToolCall{CallID: "probe", Name: "probe", Arguments: `{}`}), testkit.Step{Gate: gate, Text: "late"})
	main := testkit.NewFake()
	opts.Model, opts.Agents[0].Model = versionedPauseModel{main}, versionedPauseModel{child}
	s := openSubagentSession(t, opts, false)
	v := s.rt.manager.View()
	inv := onlyInvocation(t, v)
	if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: in.TraceID, ExpectedRevision: v.LastSeq}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return effects.Load() == 1 && child.Calls() == 2 })
	if err := s.Cancel(t.Context(), in.TraceID); err != nil {
		t.Fatal(err)
	}
	v = s.rt.manager.View()
	parent := v.Calls[inv.ParentCallID]
	if v.Invocations[inv.ID].State != "cancelled" || parent.Observation == nil || parent.Observation.SideEffect != "none" || v.HasUnresolvedEffects() || effects.Load() != 1 || main.Calls() != 0 {
		t.Fatalf("known cancellation lost outcome: inv=%+v parent=%+v", v.Invocations[inv.ID], parent)
	}
}

func TestInterruptedChildResumeUsesFrozenTaskWithoutRerunningHooks(t *testing.T) {
	def, _ := delegateDefinition([]agent.AgentDefinition{{Name: "worker", Delegable: true}})
	var hooks atomic.Int32
	def.PrepareArguments = []func(context.Context, json.RawMessage) (json.RawMessage, error){func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		hooks.Add(1)
		var args map[string]string
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
		args["task"] = "prepared work"
		return json.Marshal(args)
	}}
	opts, in := stopChild(t, nil, []tools.Definition{def})
	child := &childContinuationModel{versionedPauseModel: versionedPauseModel{testkit.NewFake(testkit.Step{Text: "child done"})}}
	main := testkit.NewFake(testkit.Step{Text: "parent done"})
	opts.Model, opts.Agents[0].Model = versionedPauseModel{main}, child
	s := openSubagentSession(t, opts, false)
	v := s.rt.manager.View()
	parent := delegateRecord(t, s)
	if !strings.Contains(string(v.FrozenExecutions["execution:"+parent.Call.CallID].FinalArguments), "prepared work") {
		t.Fatal("first execution did not freeze prepared task")
	}
	if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: in.TraceID, ExpectedRevision: v.LastSeq}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	raw, _ := child.request.Load().(string)
	if !strings.Contains(raw, "prepared work") || hooks.Load() != 1 || child.Calls() != 1 {
		t.Fatalf("recovery changed actual task or reran hook: prepared=%v hooks=%d calls=%d", strings.Contains(raw, "prepared work"), hooks.Load(), child.Calls())
	}
}

func TestInterruptedChildResumePreservesTruncationFlag(t *testing.T) {
	opts, in := stopChild(t, nil, nil)
	child := testkit.NewFake(testkit.Step{Text: strings.Repeat("中", config.DelegateResultBytes)})
	main := testkit.NewFake(testkit.Step{Text: "parent done"})
	opts.Model, opts.Agents[0].Model = versionedPauseModel{main}, versionedPauseModel{child}
	s := openSubagentSession(t, opts, false)
	v := s.rt.manager.View()
	if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: in.TraceID, ExpectedRevision: v.LastSeq}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	out := delegateOutcome(t, delegateRecord(t, s))
	if !out.Truncated || len(out.Result) > config.DelegateResultBytes || !utf8.ValidString(out.Result) {
		t.Fatalf("recovered result hides truncation: truncated=%v bytes=%d", out.Truncated, len(out.Result))
	}
}

func TestInterruptedChildResumeUnknownToolBlocksParent(t *testing.T) {
	// The initial run stops before tools; the recovered child calls a tool whose
	// result is unknown. This must never become a successful parent observation.
	opts, in := stopChild(t, nil, []tools.Definition{countedTool("effect", new(atomic.Int32), errors.New("lost reply"))})
	main := testkit.NewFake(testkit.Step{Text: "never"})
	child := testkit.NewFake(toolCalls(schema.FunctionToolCall{CallID: "effect", Name: "effect", Arguments: `{}`}), testkit.Step{Text: "done"})
	opts.Model, opts.Agents[0].Model = versionedPauseModel{main}, versionedPauseModel{child}
	s := openSubagentSession(t, opts, false)
	v := s.rt.manager.View()
	_, err := s.Resume(t.Context(), ResumeCommand{TraceID: in.TraceID, ExpectedRevision: v.LastSeq})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	v = s.rt.manager.View()
	parent := delegateRecord(t, s)
	if parent.Observation == nil || parent.Observation.SideEffect != "unknown" || !v.HasUnresolvedEffects() || main.Calls() != 0 {
		t.Fatalf("unknown child result bypassed parent guard: observation=%+v main=%d", parent.Observation, main.Calls())
	}
	if !strings.Contains(v.Traces[in.TraceID].Error, product.CodeReconciliationRequired) {
		t.Fatalf("missing reconciliation error: %+v", v.Traces[in.TraceID])
	}
}
