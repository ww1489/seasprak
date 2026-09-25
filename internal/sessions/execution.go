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
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
)

func (rt *runtime) runSegment(frame *execution, inputID string) {
	err := rt.executeSegment(frame, inputID)
	_ = rt.do(context.Background(), func(rt *runtime) error { rt.segmentFinished(frame, err); return nil })
}
func (rt *runtime) executeSegment(frame *execution, inputID string) error {
	ctx := frame.ctx
	scope := frame.scope
	exec, err := tools.NewExecutor(scope.Generation, rt.opts.Tools, rt, allowAll{}, frame.budget)
	if err != nil {
		return err
	}
	baseTools := make([]tool.BaseTool, 0, len(rt.opts.ToolInfos))
	for _, info := range rt.opts.ToolInfos {
		baseTools = append(baseTools, einorun.NewPipelineTool(info, exec, scope))
	}
	ag, err := einorun.NewAgent(ctx, einorun.Deps{Model: rt.opts.Model, Tools: baseTools, Sink: rt, Budget: frame.budget, Boundary: rt, Instruction: rt.opts.Instruction, Scope: scope})
	if err != nil {
		return err
	}
	var eventErr error
	loop := adk.NewTurnLoop(adk.TurnLoopConfig[agent.InputRef, *schema.AgenticMessage]{
		GenInput: func(ctx context.Context, _ *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], items []agent.InputRef) (*adk.GenInputResult[agent.InputRef, *schema.AgenticMessage], error) {
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
			return &adk.GenInputResult[agent.InputRef, *schema.AgenticMessage]{Input: &adk.TypedAgentInput[*schema.AgenticMessage]{Messages: value.([]*schema.AgenticMessage)}, Consumed: items[:1], Remaining: items[1:], RunOpts: []adk.AgentRunOption{adk.WithAfterToolCallsHook(einorun.FinishAfterTools(rt, scope))}}, nil
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
	if pushed, _ := loop.Push(agent.InputRef{InputID: inputID, TraceID: scope.TraceID, Kind: "prompt"}); !pushed {
		return product.NewError(product.CodeStateConflict, "execution loop rejected input")
	}
	loop.Run(ctx)
	exit := loop.Wait()
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
func (rt *runtime) CommitFact(ctx context.Context, scope agent.ExecutionScope, fact agent.Fact) error {
	// Execution results must be recorded even when the request was cancelled.
	return rt.do(context.WithoutCancel(ctx), func(rt *runtime) error {
		if rt.active == nil || rt.active.scope.TraceID != scope.TraceID || rt.active.scope.InvocationID != scope.InvocationID {
			return product.NewError(product.CodeStateConflict, "execution scope is not active")
		}
		if scope.TurnID == "" {
			scope.TurnID = rt.active.turnID
		}
		switch fact.Kind {
		case "assistant":
			var body struct {
				Status  string                 `json:"status"`
				Message *schema.AgenticMessage `json:"message"`
			}
			if err := json.Unmarshal(fact.Payload, &body); err != nil {
				return err
			}
			if body.Status != "complete" {
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
			return rt.manager.SaveAssistant(ctx, msg, calls)
		case "tool_intent":
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
			if !ok || call.Scope.TurnID != scope.TurnID || call.Call != frozen {
				return product.NewError(product.CodeStateConflict, "tool intent does not match accepted call")
			}
			if call.Claimed {
				return product.NewError(product.CodeReconciliationRequired, "tool execution was already claimed")
			}
			call.Claimed = true
			return rt.manager.SaveCall(ctx, call)
		case "tool_observation":
			var call agent.ToolRecord
			if err := json.Unmarshal(fact.Payload, &call); err != nil {
				return err
			}
			if call.Scope.TurnID == "" {
				call.Scope.TurnID = scope.TurnID
			}
			old, ok := rt.manager.View().Calls[call.Call.CallID]
			if !ok || old.Call != call.Call || old.Scope != call.Scope || call.Observation == nil {
				return product.NewError(product.CodeStateConflict, "tool observation does not match accepted call")
			}
			return rt.manager.SaveCall(context.Background(), call)
		default:
			_, err := rt.manager.AppendEvent(context.Background(), fact.Kind, scope.TraceID, fact.Payload)
			return err
		}
	})
}
func assistantMessage(scope agent.ExecutionScope, msg *schema.AgenticMessage) agent.AgentMessage {
	return agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: scope.SessionID, TraceID: scope.TraceID, TurnID: scope.TurnID, InvocationID: scope.InvocationID}, Standard: msg}
}
func (rt *runtime) LookupTool(ctx context.Context, scope agent.ExecutionScope, providerID string) (agent.ToolRecord, error) {
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		if rt.active == nil || rt.active.scope.TraceID != scope.TraceID || rt.active.scope.InvocationID != scope.InvocationID {
			return nil, product.NewError(product.CodeStateConflict, "tool execution is not active")
		}
		if scope.TurnID == "" {
			scope.TurnID = rt.active.turnID
		}
		for _, call := range rt.manager.View().Calls {
			if call.Scope.TraceID == scope.TraceID && call.Scope.TurnID == scope.TurnID && call.Call.ProviderCallID == providerID {
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
func (rt *runtime) PrepareNextTurn(ctx context.Context, scope agent.ExecutionScope) (agent.TurnPlan, error) {
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		if rt.active == nil || rt.active.scope.TraceID != scope.TraceID {
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
		turn := agent.TurnRecord{ID: agent.MustID(), TraceID: scope.TraceID, InvocationID: scope.InvocationID}
		if err := rt.manager.SaveTurn(ctx, turn); err != nil {
			return nil, err
		}
		rt.active.turnID = turn.ID
		return agent.TurnPlan{TurnID: turn.ID, SelectionRevision: rt.manager.View().LastSeq}, nil
	})
	if err != nil {
		return agent.TurnPlan{}, err
	}
	return value.(agent.TurnPlan), nil
}
func (rt *runtime) ShouldStop(ctx context.Context, scope agent.ExecutionScope) (bool, string, error) {
	value, err := rt.call(context.WithoutCancel(ctx), func(rt *runtime) (any, error) {
		if rt.active == nil || rt.active.scope.TraceID != scope.TraceID {
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
		if rt.active == nil || rt.active.scope.TraceID != scope.TraceID {
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
		if !ok || turn.TraceID != scope.TraceID {
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
