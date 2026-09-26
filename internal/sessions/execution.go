package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	product "github.com/ww1489/seasprak/internal/errors"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

type checkpointResult struct {
	ref   agent.CheckpointBlobRef
	valid bool
}

func (rt *runtime) runSegment(frame *execution, inputID string) {
	frame.input = agent.InputRef{InputID: inputID, TraceID: frame.scope.TraceID, Kind: "prompt"}
	err := rt.beginActivity(frame)
	if err == nil {
		err = rt.executeSegment(frame, inputID)
	}
	err = errors.Join(err, rt.endActivity(frame))
	_ = rt.do(context.Background(), func(rt *runtime) error { rt.segmentFinished(frame, err); return nil })
}
func (rt *runtime) executeSegment(frame *execution, inputID string) error {
	ctx := frame.ctx
	scope := frame.scope
	environment, workspace := rt.resourceDomain()
	exec, err := tools.NewExecutor(scope.Generation, rt.opts.Tools, rt, sessionAuthorizer{rt: rt, scope: scope}, frame.budget,
		tools.WithCompiledSchemas(rt.opts.compiledTools), tools.WithOperations(rt.opts.Operations), tools.WithResourceScheduler(rt.resourceScheduler()), tools.WithResourceDomain(environment, workspace))
	if err != nil {
		return err
	}
	baseTools := make([]tool.BaseTool, 0, len(rt.opts.ToolInfos))
	for _, info := range rt.opts.ToolInfos {
		kind := ""
		for _, def := range rt.opts.Tools {
			if def.Name == info.Name {
				kind = def.ToolInterface
				break
			}
		}
		wrapped, wrapErr := einorun.NewPipelineToolForInterface(info, exec, scope, kind)
		if wrapErr != nil {
			return wrapErr
		}
		baseTools = append(baseTools, wrapped)
	}
	ag, err := einorun.NewAgent(ctx, einorun.Deps{Model: rt.opts.Model, Tools: baseTools, Sink: rt, Budget: frame.budget, Boundary: rt, Instruction: rt.opts.Instruction, Scope: scope, RemainingActivity: frame.activity.remaining,
		EmptyInventoryCall: func(ctx context.Context, callID, name, arguments string) error {
			_, err := exec.RejectUnavailable(ctx, einorun.ScopeFromContext(ctx, scope), callID, name, arguments)
			return err
		},
		UnknownToolsHandler: func(ctx context.Context, name, arguments string) (string, error) {
			out, err := exec.RejectUnavailable(ctx, einorun.ScopeFromContext(ctx, scope), compose.GetToolCallID(ctx), name, arguments)
			if err != nil {
				return "", err
			}
			raw, _ := json.Marshal(out)
			return string(raw), nil
		},
	})
	if err != nil {
		return err
	}
	var eventErr error
	var checkpoint *einorun.CheckpointStore
	var checkpointID string
	if blobs, ok := rt.opts.Store.(store.CheckpointBlobs); ok {
		checkpoint = einorun.NewCheckpointStore(checkpointBlobs{store: blobs, session: scope.SessionID})
		checkpointID = scope.ExecutionID
		if frame.resume != nil {
			checkpoint.Bind(checkpointID, agent.CheckpointBlobRef{Hash: frame.resume.BlobHash, Size: frame.resume.BlobSize})
		}
	}
	loop := adk.NewTurnLoop(adk.TurnLoopConfig[agent.InputRef, *schema.AgenticMessage]{
		Store: checkpoint, CheckpointID: checkpointID,
		GenInput: func(ctx context.Context, _ *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], items []agent.InputRef) (*adk.GenInputResult[agent.InputRef, *schema.AgenticMessage], error) {
			if frame.resume != nil {
				return nil, incompatibleResume("resume cannot fall back to a fresh input")
			}
			if len(items) == 0 {
				return nil, product.NewError(product.CodeInternal, "execution input is missing")
			}
			value, err := rt.call(ctx, func(rt *runtime) (any, error) {
				if rt.active != frame {
					return nil, product.NewError(product.CodeStateConflict, "execution is no longer active")
				}
				// A steering-only segment leaves consumption to BeforeModel so
				// each model boundary consumes exactly one steering input.
				in := rt.manager.View().Inputs[items[0].InputID]
				if in == nil || in.Kind != "steering" {
					if err := rt.manager.Consume(ctx, items[0].InputID); err != nil {
						return nil, err
					}
				}
				return agent.ConvertToLLM(rt.manager.View().Messages)
			})
			if err != nil {
				return nil, err
			}
			return &adk.GenInputResult[agent.InputRef, *schema.AgenticMessage]{RunCtx: einorun.WithExecutionScope(ctx, scope), Input: &adk.TypedAgentInput[*schema.AgenticMessage]{EnableStreaming: llm.UsesObservedTransport(rt.opts.Model), Messages: value.([]*schema.AgenticMessage)}, Consumed: items[:1], Remaining: items[1:], RunOpts: []adk.AgentRunOption{adk.WithAfterToolCallsHook(einorun.FinishAfterTools(rt, scope))}}, nil
		},
		GenResume: func(ctx context.Context, _ *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], interrupted, unhandled, newItems []agent.InputRef) (*adk.GenResumeResult[agent.InputRef, *schema.AgenticMessage], error) {
			if frame.resume == nil || len(interrupted) != 1 || interrupted[0] != frame.resume.Input || len(unhandled) != 0 || len(newItems) != 0 {
				return nil, incompatibleResume("resume input does not match the original checkpoint")
			}
			return &adk.GenResumeResult[agent.InputRef, *schema.AgenticMessage]{RunCtx: einorun.WithExecutionScope(ctx, scope), Consumed: interrupted,
				ResumeParams: &adk.ResumeParams{}, RunOpts: []adk.AgentRunOption{adk.WithAfterToolCallsHook(einorun.FinishAfterTools(rt, scope))}}, nil
		},
		PrepareAgent: func(context.Context, *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], []agent.InputRef) (adk.TypedAgent[*schema.AgenticMessage], error) {
			return ag, nil
		},
		OnAgentEvents: func(_ context.Context, tc *adk.TurnContext[agent.InputRef, *schema.AgenticMessage], events *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]) error {
			for {
				ev, ok := events.Next()
				if !ok {
					break
				}
				if ev.Err != nil && eventErr == nil {
					eventErr = ev.Err
				}
			}
			tc.Loop.Stop() // natural completion; immediate cancellation would manufacture a failure
			return eventErr
		},
	})
	if err := rt.do(context.Background(), func(rt *runtime) error {
		if rt.active != frame {
			return product.NewError(product.CodeStateConflict, "execution was replaced")
		}
		frame.loop = loop
		if frame.pauseID != "" {
			loop.Stop(adk.WithGraceful())
		}
		return nil
	}); err != nil {
		return err
	}
	// Parent cancellation (Cancel, Close, or the activity deadline) must also
	// stop the framework loop. Immediate Stop alone does not cancel synchronous
	// tool contexts. Join the callback before this segment can be finalized.
	abortDone := make(chan struct{})
	stopAbort := context.AfterFunc(ctx, func() {
		defer close(abortDone)
		einorun.AbortTurnLoop(loop, frame.cancel)
	})
	defer func() {
		if !stopAbort() {
			<-abortDone
		}
	}()
	if frame.resume == nil {
		if pushed, _ := loop.Push(frame.input); !pushed {
			return product.NewError(product.CodeStateConflict, "execution loop rejected input")
		}
	}
	loop.Run(ctx)
	exit := loop.Wait()
	pauseValue, pauseErr := rt.call(context.Background(), func(rt *runtime) (any, error) { return rt.active == frame && frame.pauseID != "", nil })
	if pauseErr != nil {
		return pauseErr
	}
	if pauseValue.(bool) {
		result := &checkpointResult{}
		frame.checkpoint = result
		var stopped *adk.CancelError
		var callbackCancel *adk.CancelError
		if eventErr != nil && !errors.As(eventErr, &callbackCancel) && !errors.Is(eventErr, einorun.ErrControlledStop) {
			return eventErr
		}
		if !exit.CheckpointAttempted || exit.CheckpointErr != nil || !errors.As(exit.ExitReason, &stopped) || len(exit.UnhandledItems) != 0 || len(exit.TakeLateItems()) != 0 || len(exit.InterruptedItems) != 1 || exit.InterruptedItems[0] != (agent.InputRef{InputID: inputID, TraceID: scope.TraceID, Kind: "prompt"}) {
			return errors.Join(exit.CheckpointErr, exit.ExitReason, product.NewError(product.CodeIncompatibleResume, "pause did not reach a matching runner checkpoint"))
		}
		ref, ok := checkpoint.Ref(checkpointID)
		if !ok {
			return product.NewError(product.CodeStorageUnavailable, "checkpoint blob is missing")
		}
		data, ok, err := checkpoint.Get(context.Background(), checkpointID)
		if err != nil || !ok {
			return errors.Join(err, product.NewError(product.CodeStorageUnavailable, "checkpoint blob cannot be read"))
		}
		if err := einorun.ValidatePausedCheckpoint(data, agent.InputRef{InputID: inputID, TraceID: scope.TraceID, Kind: "prompt"}); err != nil {
			return err
		}
		result.ref = ref
		result.valid = true
		return ctx.Err()
	}
	if exit.CheckpointErr != nil {
		return exit.CheckpointErr
	}
	if eventErr != nil && !errors.Is(eventErr, einorun.ErrControlledStop) {
		return eventErr
	}
	if exit.ExitReason != nil && !errors.Is(exit.ExitReason, einorun.ErrControlledStop) {
		return exit.ExitReason
	}
	if len(exit.UnhandledItems) > 0 || len(exit.InterruptedItems) > 0 || len(exit.TakeLateItems()) > 0 {
		return product.NewError(product.CodeStateConflict, "execution ended with unhandled input")
	}
	return ctx.Err()
}
func (rt *runtime) matchesExecution(scope agent.ExecutionScope) bool {
	if rt.active == nil || scope.ExecutionID == "" {
		return false
	}
	current := rt.active.scope
	current.TurnID, scope.TurnID = "", ""
	return current == scope
}

func (rt *runtime) CommitFact(ctx context.Context, scope agent.ExecutionScope, fact agent.Fact) error {
	// Execution results must be recorded even when the request was cancelled.
	return rt.do(context.WithoutCancel(ctx), func(rt *runtime) error {
		if !rt.matchesExecution(scope) {
			if fact.Kind == "tool_observation" {
				return rt.recordLateObservation(context.WithoutCancel(ctx), scope, fact)
			}
			return product.NewError(product.CodeStateConflict, "execution scope is not active")
		}
		if scope.TurnID == "" {
			scope.TurnID = rt.active.turnID
		}
		switch fact.Kind {
		case "tool_output":
			return rt.publishToolOutput(ctx, scope, fact.Payload)
		case "model_stream_snapshot":
			return rt.publishModelSnapshot(ctx, scope, fact.Payload)
		case "model_attempt_started":
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := rt.active.ctx.Err(); err != nil {
				return err
			}
			var identity agent.ModelAttemptIdentity
			if err := json.Unmarshal(fact.Payload, &identity); err != nil {
				return err
			}
			v := rt.manager.View()
			turn, ok := v.Turns[scope.TurnID]
			if !ok || turn.Ended || identity.ID == "" || identity.MessageID == "" || identity.StreamID == "" || identity.ModelCallID != scope.TurnID {
				return product.NewError(product.CodeStateConflict, "model attempt does not belong to active turn")
			}
			ordinal := uint64(1)
			for _, prior := range v.ModelAttempts {
				if prior.ModelCallID == identity.ModelCallID {
					ordinal++
				}
			}
			if err := rt.manager.SaveRecords(ctx, v.LastSeq, state.Records{ModelAttempts: []state.ModelAttempt{{ID: identity.ID, ModelCallID: identity.ModelCallID, MessageID: identity.MessageID, StreamID: identity.StreamID, Scope: scope, Purpose: "agent", Attempt: ordinal, State: "started", ModelConfigVersion: identity.ModelConfigVersion}}}); err != nil {
				return err
			}
			rt.publishModelStarted(scope, identity)
			return nil
		case "assistant":
			var body struct {
				AttemptID string                    `json:"attemptId"`
				Status    string                    `json:"status"`
				Message   *schema.AgenticMessage    `json:"message"`
				Details   agent.ModelAttemptDetails `json:"details"`
			}
			if err := json.Unmarshal(fact.Payload, &body); err != nil {
				return err
			}
			if body.Status != "complete" {
				if body.AttemptID != "" {
					var partial *agent.AgentMessage
					if body.Message != nil {
						msg := assistantMessage(scope, body.Message)
						msg.ID = rt.manager.View().ModelAttempts[body.AttemptID].MessageID
						msg.Status = agent.StatusIncomplete
						partial = &msg
					}
					return rt.saveAttemptResult(context.WithoutCancel(ctx), scope, body.AttemptID, body.Status, modelFinish(body.Message), partial, nil, body.Details)
				}
				if body.Message == nil {
					_, err := rt.manager.AppendEvent(context.Background(), "model.attempt_failed", scope.TraceID, fact.Payload)
					return err
				}
				msg := assistantMessage(scope, body.Message)
				msg.Status = agent.StatusIncomplete
				return rt.manager.AppendMessage(context.Background(), msg)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := rt.active.ctx.Err(); err != nil {
				return err
			}
			if body.Message == nil {
				return product.NewError(product.CodeInvalidArgument, "assistant response is empty")
			}
			msg := assistantMessage(scope, body.Message)
			calls := []agent.ToolRecord{}
			for _, block := range body.Message.ContentBlocks {
				if block == nil || block.FunctionToolCall == nil {
					continue
				}
				call := block.FunctionToolCall
				version := ""
				for _, def := range rt.opts.Tools {
					if def.Name == call.Name {
						version = def.Version
						break
					}
				}
				hash := sha256.Sum256([]byte(call.Name + "\n" + version + "\n" + call.Arguments + "\n" + scope.Generation))
				calls = append(calls, agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: agent.MustID(), ProviderCallID: call.CallID, Name: call.Name, Arguments: call.Arguments, Generation: scope.Generation, Hash: hex.EncodeToString(hash[:])}})
			}
			if body.AttemptID != "" {
				msg.ID = rt.manager.View().ModelAttempts[body.AttemptID].MessageID
				return rt.saveAttemptResult(ctx, scope, body.AttemptID, "accepted", modelFinish(body.Message), &msg, calls, body.Details)
			}
			return rt.manager.SaveAssistant(ctx, msg, calls)
		case "tool_frozen":
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := rt.active.ctx.Err(); err != nil {
				return err
			}
			var frozen agent.FrozenExecution
			if err := json.Unmarshal(fact.Payload, &frozen); err != nil {
				return err
			}
			return rt.saveFrozenExecution(ctx, scope, frozen)
		case "tool_intent":
			if rt.active.activity == nil {
				return activityExhausted()
			}
			if err := rt.active.activity.allowed(); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := rt.active.ctx.Err(); err != nil {
				return err
			}
			var frozen agent.FrozenCall
			if err := json.Unmarshal(fact.Payload, &frozen); err != nil {
				return err
			}
			call, ok := rt.manager.View().Calls[frozen.CallID]
			if !ok || call.Scope.TraceID != scope.TraceID || call.Scope.InvocationID != scope.InvocationID || call.Scope.TurnID != scope.TurnID || call.Call != frozen {
				return product.NewError(product.CodeStateConflict, "tool intent does not match accepted call")
			}
			if !acceptedAttemptForCall(rt.manager.View(), call) {
				return product.NewError(product.CodeStateConflict, "tool attempt was not accepted")
			}
			committed, ok := rt.manager.View().FrozenExecutions["execution:"+frozen.CallID]
			if !ok {
				return product.NewError(product.CodePermissionDenied, "tool execution has no frozen description")
			}
			decision, err := rt.checkToolPolicy(ctx, call.Scope, committed)
			if err != nil {
				return err
			}
			if decision != agent.DecisionAllow {
				return product.NewError(product.CodePermissionDenied, "tool execution is not allowed")
			}
			if call.Claimed {
				return product.NewError(product.CodeReconciliationRequired, "tool execution was already claimed")
			}
			if fact.Budget == nil {
				return product.NewError(product.CodeInvalidArgument, "tool intent requires atomic budget")
			}
			return rt.manager.ClaimTool(ctx, frozen, *fact.Budget)
		case "tool_observation":
			var call agent.ToolRecord
			if err := json.Unmarshal(fact.Payload, &call); err != nil {
				return err
			}
			if call.Scope.TurnID == "" {
				call.Scope.TurnID = scope.TurnID
			}
			old, ok := rt.manager.View().Calls[call.Call.CallID]
			if !ok || old.Call != call.Call || old.Scope != call.Scope || call.Observation == nil || call.Scope.TraceID != scope.TraceID || call.Scope.InvocationID != scope.InvocationID || call.Scope.TurnID != scope.TurnID {
				return product.NewError(product.CodeStateConflict, "tool observation does not match accepted call")
			}
			return rt.manager.SaveCall(context.Background(), call)
		default:
			_, err := rt.manager.AppendEvent(context.Background(), fact.Kind, scope.TraceID, fact.Payload)
			return err
		}
	})
}
func modelFinish(msg *schema.AgenticMessage) string {
	if msg == nil {
		return ""
	}
	reason, _ := msg.Extra["seasprak.finish"].(string)
	return reason
}

func assistantMessage(scope agent.ExecutionScope, msg *schema.AgenticMessage) agent.AgentMessage {
	return agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: scope.SessionID, TraceID: scope.TraceID, TurnID: scope.TurnID, InvocationID: scope.InvocationID}, Standard: msg}
}
func (rt *runtime) LookupTool(ctx context.Context, scope agent.ExecutionScope, providerID string) (agent.ToolRecord, error) {
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		if !rt.matchesExecution(scope) {
			return nil, product.NewError(product.CodeStateConflict, "tool execution is not active")
		}
		if scope.TurnID == "" {
			scope.TurnID = rt.active.turnID
		}
		view := rt.manager.View()
		for _, call := range view.Calls {
			if call.Scope.TraceID == scope.TraceID && call.Scope.InvocationID == scope.InvocationID && call.Scope.TurnID == scope.TurnID && call.Call.ProviderCallID == providerID {
				if !acceptedAttemptForCall(view, call) {
					return nil, product.NewError(product.CodeStateConflict, "tool attempt was not accepted")
				}
				return call, nil
			}
		}
		return nil, product.NewError(product.CodeStateConflict, "tool call was not accepted")
	})
	if err != nil {
		return agent.ToolRecord{}, err
	}
	return value.(agent.ToolRecord), nil
}

// Legacy P1 journals have no attempt records. Once a Turn has registered an
// attempt, only its accepted candidate can authorize the matching model call.
func acceptedAttemptForCall(view state.View, call agent.ToolRecord) bool {
	registered := false
	for id, initial := range view.ModelAttempts {
		if initial.Scope.TraceID != call.Scope.TraceID || initial.Scope.InvocationID != call.Scope.InvocationID || initial.Scope.TurnID != call.Scope.TurnID {
			continue
		}
		registered = true
		if view.AttemptResults[id].State != "accepted" || initial.Scope != call.Scope {
			continue
		}
		for _, msg := range view.Messages {
			if msg.ID != initial.MessageID || msg.Status != agent.StatusComplete || msg.Standard == nil {
				continue
			}
			for _, block := range msg.Standard.ContentBlocks {
				if block == nil || block.FunctionToolCall == nil {
					continue
				}
				fc := block.FunctionToolCall
				if fc.CallID == call.Call.ProviderCallID && fc.Name == call.Call.Name && fc.Arguments == call.Call.Arguments {
					return true
				}
			}
		}
	}
	return !registered
}

func (rt *runtime) PrepareNextTurn(ctx context.Context, scope agent.ExecutionScope) (agent.TurnPlan, error) {
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		if !rt.matchesExecution(scope) {
			return nil, product.NewError(product.CodeStateConflict, "trace is not active")
		}
		if err := rt.active.ctx.Err(); err != nil {
			return nil, err
		}
		if rt.active.turnID != "" {
			previous := rt.manager.View().Turns[rt.active.turnID]
			if !previous.Ended {
				return nil, product.NewError(product.CodeStateConflict, "previous turn is unfinished")
			}
		}
		// Allocation is not a durable turn start. BeginTurnID persists this ID
		// together with logical occupancy before any model request is allowed.
		turn := agent.TurnRecord{ID: agent.MustID(), TraceID: scope.TraceID, InvocationID: scope.InvocationID}
		rt.active.turnID = turn.ID
		rt.active.turnSelectionRevision = rt.manager.View().LastSeq
		return agent.TurnPlan{TurnID: turn.ID, SelectionRevision: rt.active.turnSelectionRevision}, nil
	})
	if err != nil {
		return agent.TurnPlan{}, err
	}
	return value.(agent.TurnPlan), nil
}
func (rt *runtime) ShouldStop(ctx context.Context, scope agent.ExecutionScope) (bool, string, error) {
	value, err := rt.call(context.WithoutCancel(ctx), func(rt *runtime) (any, error) {
		if !rt.matchesExecution(scope) {
			return "execution_stopped", nil
		}
		if rt.active.ctx.Err() != nil {
			return "cancelled", nil
		}
		view := rt.manager.View()
		if view.HasUnresolvedEffects() {
			return product.CodeReconciliationRequired, nil
		}
		tr := view.Traces[scope.TraceID]
		if tr.State != "running" {
			return tr.State, nil
		}
		return "", nil
	})
	if err != nil {
		return false, "", err
	}
	reason := value.(string)
	return reason != "", reason, nil
}
func (rt *runtime) FinishTurn(ctx context.Context, scope agent.ExecutionScope, fact agent.TurnFact) error {
	return rt.do(context.WithoutCancel(ctx), func(rt *runtime) error {
		if !rt.matchesExecution(scope) {
			return product.NewError(product.CodeStateConflict, "trace is not active")
		}
		id := fact.TurnID
		if id == "" {
			id = scope.TurnID
		}
		if id == "" {
			id = rt.active.turnID
		}
		turn, ok := rt.manager.View().Turns[id]
		if !ok || turn.TraceID != scope.TraceID || turn.InvocationID != scope.InvocationID || id != rt.active.turnID {
			return product.NewError(product.CodeStateConflict, "turn is not active")
		}
		if len(turn.CallIDs) > 0 {
			return rt.manager.FinishTools(context.Background(), turn)
		}
		turn.Ended = true
		return rt.manager.SaveTurn(context.Background(), turn)
	})
}
func (rt *runtime) TakeSteering(ctx context.Context, scope agent.ExecutionScope) (*agent.AgentMessage, error) {
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		if !rt.matchesExecution(scope) {
			return nil, product.NewError(product.CodeStateConflict, "execution is no longer active")
		}
		if err := rt.active.ctx.Err(); err != nil {
			return nil, err
		}
		v := rt.manager.View()
		for _, id := range v.Steering {
			in := v.Inputs[id]
			if in.TraceID != scope.TraceID || in.State != "pending" {
				continue
			}
			if err := rt.manager.Consume(ctx, id); err != nil {
				return nil, err
			}
			messages := rt.manager.View().Messages
			msg := messages[len(messages)-1]
			return &msg, nil
		}
		return nil, nil
	})
	if err != nil || value == nil {
		return nil, err
	}
	return value.(*agent.AgentMessage), nil
}
func (rt *runtime) finishInterruptedTurn(frame *execution) error {
	if frame.turnID == "" {
		return nil
	}
	view := rt.manager.View()
	turn, ok := view.Turns[frame.turnID]
	if !ok || turn.Ended {
		return nil
	}
	for _, id := range turn.CallIDs {
		call := view.Calls[id]
		if call.Observation != nil {
			continue
		}
		status, content, effect := "skipped", "execution stopped before tool started", "none"
		if call.Claimed {
			status, content, effect = "outcome_unknown", "execution stopped without a durable result", "unknown"
		}
		call.Observation = &agent.ToolObservation{Status: status, Content: content, SideEffect: effect, Executed: call.Claimed}
		if err := rt.manager.SaveCall(context.Background(), call); err != nil {
			return err
		}
	}
	if len(turn.CallIDs) > 0 {
		return rt.manager.FinishTools(context.Background(), turn)
	}
	turn.Ended = true
	return rt.manager.SaveTurn(context.Background(), turn)
}
