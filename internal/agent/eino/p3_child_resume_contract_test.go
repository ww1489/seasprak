package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/testkit"
)

// These probes exercise the pinned native bridge with product NewAgent and
// ChildContext. They characterize framework behavior, not Session recovery.
// The parent store is the only store: AgentTool embeds every descendant's
// opaque runner state in the parent's serialized checkpoint.
type p3ChildProbe struct {
	models      map[string]*p3ChildModel
	scope       agent.ExecutionScope
	a, b        atomic.Int32
	done        atomic.Int32
	doneEntries atomic.Int32
	block       *p3ChildBlock
	bEntered    chan struct{}
	doneEntered chan struct{}
}

type p3ChildBlock struct {
	entered, gate, exited chan struct{}
	cancelObserved        chan struct{}
	ignoreCancellation    bool
	once                  sync.Once
}

type p3ChildModel struct {
	*testkit.FakeModel
	name            string
	task            string
	scope           agent.ExecutionScope
	calls           []schema.FunctionToolCall
	results         []schema.FunctionToolResult
	effect          *atomic.Int32
	generates       atomic.Int32
	streams         atomic.Int32
	metadataSeen    atomic.Int32
	delegations     atomic.Int32
	approvalEntries atomic.Int32
}

func p3ChildRootScope() agent.ExecutionScope {
	return agent.ExecutionScope{SessionID: "probe", BranchID: "branch", TraceID: "trace", InvocationID: "root", ParentInvocationID: "probe-owner", ExecutionID: "first", Generation: "generation", TurnID: "probe-turn", SelectionRevision: 17}
}

// These are test invocation identities derived from the original delegate
// provider call, not product identities and not native agent names or RunPath.
func p3ChildScope(parent agent.ExecutionScope, providerCallID string) agent.ExecutionScope {
	child := parent
	child.InvocationID = "probe-invocation:" + parent.InvocationID + ":" + providerCallID
	child.ParentInvocationID = parent.InvocationID
	return child
}

func p3DelegateCall(id, target, task string) schema.FunctionToolCall {
	return schema.FunctionToolCall{CallID: id, Name: "delegate_task", Arguments: fmt.Sprintf(`{"agent":%q,"task":%q}`, target, task)}
}

func p3ToolResult(id, name, content string) schema.FunctionToolResult {
	return schema.FunctionToolResult{CallID: id, Name: name, Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: content}}}}
}

func p3NewChildModel(name, task string, scope agent.ExecutionScope, calls []schema.FunctionToolCall, results []schema.FunctionToolResult, final string) *p3ChildModel {
	return &p3ChildModel{name: name, task: task, scope: scope, calls: calls, results: results, FakeModel: testkit.NewFake(testkit.Step{Finish: "tool_calls", ToolCalls: calls}, testkit.Step{Text: final})}
}

func p3ChildMetadata(invocationID string) map[string]any {
	return map[string]any{"owner": invocationID, "items": []any{"synthetic", float64(17)}, "extension": map[string]any{"signed": "probe-only"}}
}

func (m *p3ChildModel) generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	if err := m.checkInput(ctx, in); err != nil {
		return nil, err
	}
	msg, err := m.FakeModel.Generate(ctx, in, opts...)
	if msg != nil {
		msg.Extra["p3-child-contract"] = p3ChildMetadata(m.scope.InvocationID)
	}
	return msg, err
}

func (m *p3ChildModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	m.generates.Add(1)
	return m.generate(ctx, in, opts...)
}

func (m *p3ChildModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.streams.Add(1)
	msg, err := m.generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

func (m *p3ChildModel) checkInput(ctx context.Context, in []*schema.AgenticMessage) error {
	if got := ScopeFromContext(ctx, agent.ExecutionScope{}); got != m.scope {
		return fmt.Errorf("%s full execution scope differs: got=%+v want=%+v", m.name, got, m.scope)
	}
	var messages []*schema.AgenticMessage
	for _, msg := range in {
		if msg == nil {
			return errors.New("nil message in child input")
		}
		if msg.Role != schema.AgenticRoleTypeSystem {
			messages = append(messages, msg)
		}
	}
	request := m.Calls()
	wantMessages := 1
	if request == 1 {
		wantMessages = 2 + len(m.results)
	}
	if request > 1 || len(messages) != wantMessages {
		return fmt.Errorf("%s request %d has %d messages, want %d", m.name, request+1, len(messages), wantMessages)
	}
	initial := messages[0]
	if initial.Role != schema.AgenticRoleTypeUser || len(initial.ContentBlocks) != 1 || initial.ContentBlocks[0] == nil || initial.ContentBlocks[0].Type != schema.ContentBlockTypeUserInputText || !reflect.DeepEqual(initial.ContentBlocks[0].UserInputText, &schema.UserInputText{Text: m.task}) {
		return errors.New("missing, duplicated, or foreign initial child task")
	}
	if request == 0 {
		return nil
	}
	assistant := messages[1]
	if assistant.Role != schema.AgenticRoleTypeAssistant || len(assistant.ContentBlocks) != len(m.calls) || !reflect.DeepEqual(assistant.Extra["p3-child-contract"], p3ChildMetadata(m.scope.InvocationID)) {
		return errors.New("checkpoint lost original assistant calls or invocation metadata")
	}
	for i, call := range m.calls {
		block := assistant.ContentBlocks[i]
		if block == nil || block.Type != schema.ContentBlockTypeFunctionToolCall || !reflect.DeepEqual(block.FunctionToolCall, &call) {
			return fmt.Errorf("%s original assistant call %d identity, arguments, or order differs", m.name, i)
		}
	}
	for i, result := range m.results {
		msg := messages[i+2]
		if msg.Role != schema.AgenticRoleTypeUser || len(msg.ContentBlocks) != 1 || msg.ContentBlocks[0] == nil || msg.ContentBlocks[0].Type != schema.ContentBlockTypeFunctionToolResult || !reflect.DeepEqual(msg.ContentBlocks[0].FunctionToolResult, &result) {
			return fmt.Errorf("%s result %d identity, name, content, or order differs", m.name, i)
		}
	}
	m.metadataSeen.Add(1)
	return nil
}

func newP3ChildProbe() *p3ChildProbe {
	p := &p3ChildProbe{scope: p3ChildRootScope()}
	aScope, bScope := p3ChildScope(p.scope, "root-a"), p3ChildScope(p.scope, "root-b")
	leafScope := p3ChildScope(aScope, "a-nested")
	p.models = map[string]*p3ChildModel{
		"root": p3NewChildModel("root", "task:root", p.scope, []schema.FunctionToolCall{p3DelegateCall("root-a", "a", "task:a"), p3DelegateCall("root-b", "b", "task:b"), {CallID: "root-done", Name: "done", Arguments: `{}`}}, []schema.FunctionToolResult{p3ToolResult("root-a", "delegate_task", "a-result"), p3ToolResult("root-b", "delegate_task", "b-result"), p3ToolResult("root-done", "done", "done-result")}, "root-result"),
		"a":    p3NewChildModel("a", "task:a", aScope, []schema.FunctionToolCall{p3DelegateCall("a-nested", "leaf", "task:leaf")}, []schema.FunctionToolResult{p3ToolResult("a-nested", "delegate_task", "leaf-result")}, "a-result"),
		"leaf": p3NewChildModel("leaf", "task:leaf", leafScope, []schema.FunctionToolCall{{CallID: "leaf-ask", Name: "approve", Arguments: `{}`}}, []schema.FunctionToolResult{p3ToolResult("leaf-ask", "approve", "leaf-approved")}, "leaf-result"),
		"b":    p3NewChildModel("b", "task:b", bScope, []schema.FunctionToolCall{{CallID: "b-ask", Name: "approve", Arguments: `{}`}}, []schema.FunctionToolResult{p3ToolResult("b-ask", "approve", "b-approved")}, "b-result"),
	}
	p.models["leaf"].effect, p.models["b"].effect = &p.a, &p.b
	return p
}

func (p *p3ChildProbe) modelForScope(ctx context.Context, name string) *p3ChildModel {
	scope := ScopeFromContext(ctx, agent.ExecutionScope{})
	for _, m := range p.models {
		if m.name == name && m.scope == scope {
			return m
		}
	}
	return nil
}

// The dynamic wrapper preserves the product's delegate_task(agent,task) input
// instead of exposing one native tool per agent. Options and context must reach
// AgentTool unchanged so its recursive stop/resume bridge can operate.
type p3DynamicChildTool struct{ probe *p3ChildProbe }

func (d *p3DynamicChildTool) Info(context.Context) (*schema.ToolInfo, error) {
	return testkit.ToolInfo("delegate_task", "Explicit child task"), nil
}

func (d *p3DynamicChildTool) InvokableRun(ctx context.Context, raw string, opts ...tool.Option) (string, error) {
	var args struct{ Agent, Task string }
	providerCallID := compose.GetToolCallID(ctx)
	if json.Unmarshal([]byte(raw), &args) != nil || providerCallID == "" {
		return "", errors.New("invalid probe delegate call")
	}
	parent := ScopeFromContext(ctx, agent.ExecutionScope{})
	child := p3ChildScope(parent, providerCallID)
	childCtx := ChildContext(ctx, child)
	m := d.probe.modelForScope(childCtx, args.Agent)
	if m == nil || args.Task != m.task {
		return "", errors.New("probe invocation, target, or task differs")
	}
	// Deliberately update the child's mutable box. The outer context and a
	// concurrently running sibling must retain their own full scope.
	mutated := child
	mutated.TurnID, mutated.SelectionRevision = "mutable:"+child.InvocationID, child.SelectionRevision+1
	noteScope(childCtx, mutated)
	if ScopeFromContext(childCtx, agent.ExecutionScope{}) != mutated || ScopeFromContext(ctx, agent.ExecutionScope{}) != parent {
		return "", errors.New("child mutable scope was not isolated")
	}
	noteScope(childCtx, child)
	m.delegations.Add(1)
	ag, err := d.probe.newAgent(childCtx, args.Agent)
	if err != nil {
		return "", err
	}
	request, _ := json.Marshal(map[string]string{"request": args.Task})
	out, err := adk.NewTypedAgentTool(childCtx, ag).(tool.InvokableTool).InvokableRun(childCtx, string(request), opts...)
	if ScopeFromContext(ctx, agent.ExecutionScope{}) != parent {
		return "", errors.New("child rewrote parent mutable scope")
	}
	return out, err
}

type p3ApprovalProbeTool struct {
	model   *p3ChildModel
	entered chan struct{}
}

func (p *p3ApprovalProbeTool) Info(context.Context) (*schema.ToolInfo, error) {
	return testkit.ToolInfo("approve", "Approve one probe effect"), nil
}

func (p *p3ApprovalProbeTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	p.model.approvalEntries.Add(1)
	if p.entered != nil {
		close(p.entered)
	}
	interrupted, hasState, state := tool.GetInterruptState[string](ctx)
	targeted, hasAnswer, answer := tool.GetResumeContext[string](ctx)
	if interrupted && (!hasState || state != p.model.scope.InvocationID) {
		return "", errors.New("approval state collided with another invocation or native child bytes")
	}
	if targeted && hasAnswer && answer == "allowed-once" {
		p.model.effect.Add(1)
		return p.model.name + "-approved", nil
	}
	return "", tool.StatefulInterrupt(ctx, "approve:"+p.model.name, p.model.scope.InvocationID)
}

func (p *p3ChildProbe) newAgent(ctx context.Context, name string) (adk.TypedResumableAgent[*schema.AgenticMessage], error) {
	m := p.modelForScope(ctx, name)
	if m == nil {
		return nil, errors.New("no probe script for full invocation scope")
	}
	var ts []tool.BaseTool
	added := map[string]bool{}
	for _, call := range m.calls {
		if added[call.Name] {
			continue
		}
		added[call.Name] = true
		switch call.Name {
		case "delegate_task":
			ts = append(ts, &p3DynamicChildTool{p})
		case "done":
			ts = append(ts, &p2ProbeTool{name: "done", run: func(context.Context) (string, error) {
				p.doneEntries.Add(1)
				p.done.Add(1)
				if p.doneEntered != nil {
					close(p.doneEntered)
				}
				return "done-result", nil
			}})
		case "approve":
			if p.block != nil && name == "leaf" {
				ts = append(ts, &p2ProbeTool{name: "approve", run: func(ctx context.Context) (string, error) {
					defer close(p.block.exited) // the actual tool's own exit path
					m.approvalEntries.Add(1)
					p.block.once.Do(func() { close(p.block.entered) })
					if p.block.ignoreCancellation {
						stop := context.AfterFunc(ctx, func() { close(p.block.cancelObserved) })
						defer stop()
						<-p.block.gate
						return "leaf-result", nil
					}
					select {
					case <-p.block.gate:
						return "leaf-result", nil
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}})
			} else {
				var entered chan struct{}
				if name == "b" {
					entered = p.bEntered
				}
				ts = append(ts, &p3ApprovalProbeTool{model: m, entered: entered})
			}
		}
	}
	return NewAgent(ctx, Deps{Model: m, Tools: ts, Sink: &checkpointSink{}, Budget: agent.NewBudget(config.DefaultLimits()), Name: name, Instruction: "Probe " + name, Scope: m.scope})
}

func (p *p3ChildProbe) counts() map[string]int32 {
	counts := map[string]int32{"done.entries": p.doneEntries.Load(), "done.effects": p.done.Load(), "leaf.effects": p.a.Load(), "b.effects": p.b.Load()}
	for key, m := range p.models {
		counts[key+".model"] = int32(m.Calls())
		counts[key+".generate"] = m.generates.Load()
		counts[key+".stream"] = m.streams.Load()
		counts[key+".delegate"] = m.delegations.Load()
		counts[key+".approve"] = m.approvalEntries.Load()
	}
	return counts
}

func p3CollectChildProbe(it *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]) ([]*adk.InterruptCtx, error) {
	var interrupts []*adk.InterruptCtx
	var runErr error
	for {
		ev, ok := it.Next()
		if !ok {
			break
		}
		runErr = errors.Join(runErr, ev.Err)
		if ev.Output != nil && ev.Output.MessageOutput != nil {
			_, err := ev.Output.MessageOutput.GetMessage()
			runErr = errors.Join(runErr, err)
		}
		if ev.Action != nil && ev.Action.Interrupted != nil {
			interrupts = ev.Action.Interrupted.InterruptContexts
		}
	}
	return interrupts, runErr
}

func p3DrainChildProbe(t *testing.T, it *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]) []*adk.InterruptCtx {
	t.Helper()
	interrupts, err := p3CollectChildProbe(it)
	if err != nil {
		t.Fatalf("native child probe failed: %v", err)
	}
	return interrupts
}

type p3ExpectedRoot struct {
	info    string
	address adk.Address
}

func p3ProbeRoots(t *testing.T, contexts []*adk.InterruptCtx, expected map[string]p3ExpectedRoot) map[string]string {
	t.Helper()
	if len(contexts) != len(expected) {
		t.Fatalf("root cause count=%d, want %d", len(contexts), len(expected))
	}
	ids := make(map[string]string, len(expected))
	seenIDs := map[string]bool{}
	for _, ic := range contexts {
		if ic == nil || !ic.IsRootCause || ic.ID == "" || seenIDs[ic.ID] {
			t.Fatal("nil, non-root, missing, or duplicated opaque interrupt identity")
		}
		matched := ""
		for label, want := range expected {
			if reflect.DeepEqual(ic.Address, want.address) && ic.Info == want.info {
				matched = label
				break
			}
		}
		if matched == "" || ids[matched] != "" {
			t.Fatalf("foreign, duplicated, or incorrectly addressed root: info=%v address=%v", ic.Info, ic.Address)
		}
		ids[matched], seenIDs[ic.ID] = ic.ID, true
	}
	return ids
}

func p3FixedAddresses(names ...string) map[string]p3ExpectedRoot {
	all := map[string]adk.Address{
		"leaf": {{Type: adk.AddressSegmentAgent, ID: "root"}, {Type: adk.AddressSegmentTool, ID: "delegate_task", SubID: "root-a"}, {Type: adk.AddressSegmentAgent, ID: "a"}, {Type: adk.AddressSegmentTool, ID: "delegate_task", SubID: "a-nested"}, {Type: adk.AddressSegmentAgent, ID: "leaf"}, {Type: adk.AddressSegmentTool, ID: "approve", SubID: "leaf-ask"}},
		"b":    {{Type: adk.AddressSegmentAgent, ID: "root"}, {Type: adk.AddressSegmentTool, ID: "delegate_task", SubID: "root-b"}, {Type: adk.AddressSegmentAgent, ID: "b"}, {Type: adk.AddressSegmentTool, ID: "approve", SubID: "b-ask"}},
	}
	expected := make(map[string]p3ExpectedRoot, len(names))
	for _, name := range names {
		expected["approve:"+name] = p3ExpectedRoot{info: "approve:" + name, address: all[name]}
	}
	return expected
}

func TestP3ChildResumeContractRejectsMissingTaskAndWrongScope(t *testing.T) {
	p := newP3ChildProbe()
	scope := p.scope
	m := p.models["root"]
	if err := m.checkInput(WithExecutionScope(t.Context(), scope), nil); err == nil {
		t.Fatal("input certification accepted a missing original task")
	}
	scope.SessionID = "foreign-session"
	if err := m.checkInput(WithExecutionScope(t.Context(), scope), []*schema.AgenticMessage{schema.UserAgenticMessage("task:root")}); err == nil {
		t.Fatal("input certification accepted a foreign full execution scope")
	}
	if m.Calls() != 0 {
		t.Fatal("rejected input reached the scripted model")
	}
}

func TestP3ChildResumeContractCompositeCheckpoint(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming_%t", streaming), func(t *testing.T) {
			p := newP3ChildProbe()
			scope := p.scope
			ctx := WithExecutionScope(t.Context(), scope)
			store := &p3WorkflowStore{}
			makeRunner := func(store *p3WorkflowStore, enableStreaming bool) *adk.TypedRunner[*schema.AgenticMessage] {
				ag, err := p.newAgent(ctx, "root")
				if err != nil {
					t.Fatal("probe agent construction failed")
				}
				return adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag, EnableStreaming: enableStreaming, CheckPointStore: store})
			}
			contexts := p3DrainChildProbe(t, makeRunner(store, streaming).Run(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("task:root")}, adk.WithCheckPointID("tree")))
			ids := p3ProbeRoots(t, contexts, p3FixedAddresses("leaf", "b"))
			leafID, bID := ids["approve:leaf"], ids["approve:b"]
			if leafID == bID || p.done.Load() != 1 || p.a.Load() != 0 || p.b.Load() != 0 {
				t.Fatal("initial tree effects or approval identities differ")
			}
			for _, m := range p.models {
				if m.Calls() != 1 {
					t.Fatal("initial child model invocation count differs")
				}
			}
			blob, exists, err := store.Get(ctx, "tree")
			if err != nil || !exists || len(blob) == 0 {
				t.Fatal("aggregate serialized checkpoint is missing")
			}
			if ValidatePausedCheckpoint(blob, agent.InputRef{InputID: "input", TraceID: scope.TraceID, Kind: "prompt"}, "root") == nil {
				t.Fatal("raw child runner state was mistaken for a parent TurnLoop checkpoint")
			}
			reopened := &p3WorkflowStore{}
			if reopened.Set(ctx, "tree", blob) != nil {
				t.Fatal("restore aggregate checkpoint failed")
			}
			blob[0] ^= 0xff // only independently restored bytes may explain resume
			// Product scope is rebound from the caller, not serialized by Eino.
			ctx = WithExecutionScope(t.Context(), scope)
			it, err := makeRunner(reopened, !streaming).ResumeWithParams(ctx, "tree", &adk.ResumeParams{Targets: map[string]any{leafID: "allowed-once"}})
			if err != nil {
				t.Fatal("aggregate checkpoint could not reopen")
			}
			contexts = p3DrainChildProbe(t, it)
			bID = p3ProbeRoots(t, contexts, p3FixedAddresses("b"))["approve:b"]
			if p.a.Load() != 1 || p.b.Load() != 0 || p.done.Load() != 1 || p.models["root"].Calls() != 1 || p.models["b"].Calls() != 1 || p.models["a"].Calls() != 2 || p.models["leaf"].Calls() != 2 {
				t.Fatal("selective resume repeated completed work or authorized an untargeted child")
			}
			it, err = makeRunner(reopened, !streaming).ResumeWithParams(ctx, "tree", &adk.ResumeParams{Targets: map[string]any{bID: "allowed-once"}})
			if err != nil {
				t.Fatal("remaining child could not resume")
			}
			if contexts = p3DrainChildProbe(t, it); len(contexts) != 0 {
				t.Fatal("answered tree did not finish")
			}
			if p.a.Load() != 1 || p.b.Load() != 1 || p.done.Load() != 1 {
				t.Fatal("completed effects were repeated")
			}
			if p.models["a"].delegations.Load() != 2 || p.models["leaf"].delegations.Load() != 2 || p.models["b"].delegations.Load() != 3 || p.models["leaf"].approvalEntries.Load() != 2 || p.models["b"].approvalEntries.Load() != 3 || p.doneEntries.Load() != 1 {
				t.Fatal("native continuation admission counts differ")
			}
			t.Logf("fixed-tree counts: %v", p.counts())
			for _, m := range p.models {
				if m.Calls() != 2 || m.metadataSeen.Load() != 1 {
					t.Fatal("recovery repeated a request or lost original extension metadata")
				}
				wantGenerate, wantStream := int32(2), int32(0)
				if streaming {
					wantGenerate, wantStream = 0, 2
				}
				if m.generates.Load() != wantGenerate || m.streams.Load() != wantStream {
					t.Fatalf("%s changed original streaming mode: Generate=%d Stream=%d", m.name, m.generates.Load(), m.streams.Load())
				}
			}
		})
	}
}

// The recursive bridge propagates stop, but product joins and durable stopped
// proofs remain Session responsibilities.
func TestP3ChildResumeContractRecursiveStop(t *testing.T) {
	for _, graceful := range []bool{false, true} {
		t.Run(fmt.Sprintf("graceful_%t", graceful), func(t *testing.T) {
			p := newP3ChildProbe()
			p.block = &p3ChildBlock{entered: make(chan struct{}), gate: make(chan struct{}), exited: make(chan struct{})}
			p.bEntered, p.doneEntered = make(chan struct{}), make(chan struct{})
			scope := p.scope
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = WithExecutionScope(ctx, scope)
			ag, err := p.newAgent(ctx, "root")
			if err != nil {
				t.Fatal("probe construction failed")
			}
			store := &p3WorkflowStore{}
			ready, done := make(chan struct{}), make(chan struct{})
			consumerExit := make(chan bool, 1)
			type waitResult struct {
				exit       *adk.TurnLoopExitState[agent.InputRef, *schema.AgenticMessage]
				toolExited bool
			}
			waitExit := make(chan waitResult, 1)
			var stopped <-chan struct{}
			input := agent.InputRef{InputID: "input", TraceID: scope.TraceID, Kind: "prompt"}
			loop := adk.NewTurnLoop(adk.TurnLoopConfig[agent.InputRef, *schema.AgenticMessage]{
				Store: store, CheckpointID: "tree",
				GenInput: func(ctx context.Context, _ *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], items []agent.InputRef) (*adk.GenInputResult[agent.InputRef, *schema.AgenticMessage], error) {
					return &adk.GenInputResult[agent.InputRef, *schema.AgenticMessage]{RunCtx: WithExecutionScope(ctx, scope), Input: &adk.TypedAgentInput[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{schema.UserAgenticMessage("task:root")}}, Consumed: items}, nil
				},
				PrepareAgent: func(context.Context, *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], []agent.InputRef) (adk.TypedAgent[*schema.AgenticMessage], error) {
					return ag, nil
				},
				OnAgentEvents: func(ctx context.Context, tc *adk.TurnContext[agent.InputRef, *schema.AgenticMessage], it *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]) error {
					defer func() { consumerExit <- p3ProbeClosed(p.block.exited) }()
					stopped = tc.Stopped
					close(ready)
					var first error
					for {
						ev, ok := it.Next()
						if !ok {
							break
						}
						if ev.Err != nil && first == nil {
							first = ev.Err
						}
					}
					if ctx.Err() == nil {
						tc.Loop.Stop()
					}
					return first
				},
			})
			if pushed, _ := loop.Push(input); !pushed {
				t.Fatal("probe input rejected")
			}
			loop.Run(ctx)
			go func() {
				exit := loop.Wait()
				waitExit <- waitResult{exit: exit, toolExited: p3ProbeClosed(p.block.exited)}
				close(done)
			}()
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(p.block.gate) }) }
			// Register only after the execution and waiter have started. Cleanup
			// does not depend on the cancellation propagation being correct.
			t.Cleanup(func() {
				cancel()
				release()
				p3CleanupJoin(t, p.block.exited, "recursive tool cleanup")
				p3CleanupJoin(t, done, "recursive loop cleanup")
			})
			p2Await(t, ready, "root event consumer")
			p2Await(t, p.block.entered, "nested tool entry")
			p2Await(t, p.bEntered, "sibling approval entry")
			p2Await(t, p.doneEntered, "completed sibling entry")
			before := p.counts()
			for _, m := range p.models {
				if m.Calls() != 1 {
					t.Fatal("stop snapshot was taken before all models entered")
				}
			}
			if graceful {
				loop.Stop(adk.WithGraceful())
				p2Await(t, stopped, "graceful signal")
				if p3ProbeClosed(done) || p3ProbeClosed(p.block.exited) {
					t.Fatal("graceful gate did not keep the actual descendant running")
				}
				release()
			} else {
				AbortTurnLoop(loop, cancel)
			}
			p2Await(t, p.block.exited, "actual nested tool exit")
			p2Await(t, done, "root loop exit")
			result := <-waitExit
			exit := result.exit
			if !result.toolExited || !<-consumerExit {
				t.Fatal("consumer or loop.Wait returned before actual descendant exit")
			}
			if !reflect.DeepEqual(p.counts(), before) {
				t.Fatalf("stop changed whole-tree admissions or effects: before=%v after=%v", before, p.counts())
			}
			t.Logf("whole-tree stop snapshot unchanged: %v; consumer/Wait saw tool exit", before)
			if graceful {
				var ce *adk.CancelError
				if !errors.As(exit.ExitReason, &ce) || !exit.CheckpointAttempted || exit.CheckpointErr != nil {
					t.Fatal("graceful recursive stop lost checkpoint")
				}
				blob, ok, err := store.Get(t.Context(), "tree")
				if err != nil || !ok || ValidatePausedCheckpoint(blob, input, "root") != nil {
					t.Fatal("recursive pause is not a valid parent TurnLoop checkpoint")
				}
			} else {
				if exit.CheckpointAttempted {
					t.Fatal("aborted execution advertised resumable checkpoint")
				}
				if _, ok, _ := store.Get(t.Context(), "tree"); ok {
					t.Fatal("cancellation saved a checkpoint")
				}
			}
		})
	}
}
