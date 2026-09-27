package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

// CommandRequest is a trusted direct command request. It is not a model tool
// call and never creates a FunctionToolResult message.
type CommandRequest struct {
	Name             string          `json:"name"`
	Arguments        json.RawMessage `json:"arguments"`
	IdempotencyKey   string          `json:"idempotencyKey,omitempty"`
	ExpectedRevision uint64          `json:"expectedRevision,omitempty"`
}

// ExecuteCommand accepts a direct command operation. Execution itself happens
// outside the mailbox through the same Executor and controlled backend ports.
func (s *AgentSession) ExecuteCommand(ctx context.Context, request CommandRequest) (state.OperationReceipt, error) {
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		if request.Name == "" {
			return nil, product.NewError(product.CodeInvalidArgument, "command name is required")
		}
		if len(request.Arguments) == 0 {
			request.Arguments = json.RawMessage(`{}`)
		}
		// Arguments are ordinary JSON, not schemas: preserve every array's
		// order and reject trailing values before accepting an operation.
		if !json.Valid(request.Arguments) {
			return nil, product.NewError(product.CodeInvalidArgument, "command arguments are invalid JSON")
		}
		decoded, err := tools.DecodeNumbers(bytes.NewReader(request.Arguments))
		if err != nil {
			return nil, product.NewError(product.CodeInvalidArgument, "command arguments are invalid JSON")
		}
		args, err := json.Marshal(decoded)
		if err != nil {
			return nil, err
		}
		def, ok := findDefinition(rt.opts.Tools, request.Name)
		if !ok {
			return nil, product.NewError(product.CodeNotFound, "direct command is not registered")
		}
		content, err := json.Marshal(struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}{request.Name, json.RawMessage(args)})
		if err != nil {
			return nil, err
		}
		opCommand := state.OperationCommand{Principal: rt.opts.Principal, Kind: "direct_command", Target: request.Name, IdempotencyKey: request.IdempotencyKey, ExpectedRevision: request.ExpectedRevision, Content: content}
		if receipt, found, err := rt.manager.FindOperation(opCommand); found || err != nil {
			return receipt, err
		}
		if err := directBackendAvailable(rt.opts.Operations, def); err != nil {
			return nil, err
		}
		receipt, err := rt.manager.AcceptCommand(ctx, opCommand, json.RawMessage(args), agent.TargetAgent{Name: "direct-command", Version: "command-v1", Generation: rt.generation}, rt.opts.Limits)
		if err != nil {
			return nil, err
		}
		rt.schedule()
		return receipt, nil
	})
	if err != nil {
		return state.OperationReceipt{}, err
	}
	return value.(state.OperationReceipt), nil
}

type commandEnvelope struct {
	OperationID string          `json:"operationId"`
	Name        string          `json:"name"`
	Arguments   json.RawMessage `json:"arguments"`
}

func findDefinition(defs []tools.Definition, name string) (tools.Definition, bool) {
	for _, def := range defs {
		if def.Name == name {
			return def, true
		}
	}
	return tools.Definition{}, false
}

func directBackendAvailable(operations tools.Operations, def tools.Definition) error {
	backend := def.Execution.BackendID
	if backend == "" {
		backend = "trusted-run"
	}
	switch backend {
	case "process-operations":
		if operations.Process != nil {
			return nil
		}
	case "file-operations":
		if operations.Files != nil {
			return nil
		}
	case "artifact-store":
		if operations.Artifacts != nil {
			return nil
		}
	case "todo-operations":
		if operations.Todos != nil {
			return nil
		}
	case "trusted-run":
		if def.Run != nil || def.RunWithOutput != nil {
			return nil
		}
	}
	return product.NewError(product.CodeResourceUnavailable, "controlled execution backend is unavailable")
}

func errorCode(err error) string {
	if err == nil {
		return ""
	}
	if pe, ok := product.AsError(err); ok {
		return pe.Code
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return product.CodeInternal
}

func (rt *runtime) executeCommandSegment(frame *execution, inputID string) error {
	in := rt.manager.View().Inputs[inputID]
	if in == nil {
		return product.NewError(product.CodeNotFound, "direct command input is missing")
	}
	var envelope commandEnvelope
	if err := json.Unmarshal(in.Content, &envelope); err != nil || envelope.OperationID == "" || envelope.Name == "" {
		return product.NewError(product.CodeInvalidArgument, "direct command input is invalid")
	}
	if _, ok := findDefinition(rt.opts.Tools, envelope.Name); !ok {
		return product.NewError(product.CodeNotFound, "direct command is not registered")
	}
	environment, workspace := rt.resourceDomain()
	exec, err := tools.NewExecutor(frame.scope.Generation, rt.opts.Tools, rt, sessionAuthorizer{rt: rt, scope: frame.scope}, frame.budget,
		tools.WithCompiledSchemas(rt.opts.compiledTools), tools.WithOperations(rt.opts.Operations), tools.WithResourceScheduler(rt.resourceScheduler()), tools.WithResourceDomain(environment, workspace))
	if err != nil {
		return err
	}
	call := agent.ToolRecord{Scope: frame.scope, Call: agent.FrozenCall{CallID: envelope.OperationID, ProviderCallID: envelope.OperationID, OperationID: envelope.OperationID, Name: envelope.Name, Arguments: string(envelope.Arguments), Generation: frame.scope.Generation}}
	if frame.directResume == nil {
		if err := rt.do(context.Background(), func(rt *runtime) error { return rt.manager.SaveDirectCall(context.Background(), call) }); err != nil {
			return err
		}
	}
	out, runErr := exec.RunDirect(frame.ctx, frame.scope, envelope.OperationID, envelope.Name, string(envelope.Arguments))
	var wait *agent.ApprovalWait
	if errors.As(runErr, &wait) {
		return runErr
	}
	result, _ := json.Marshal(out)
	commitErr := rt.do(context.Background(), func(rt *runtime) error {
		next := "completed"
		errRef := ""
		if runErr != nil || out.Status != "succeeded" {
			next = "failed"
			errRef = errorCode(runErr)
			if errRef == "" {
				errRef = out.Status
			}
		}
		if errors.Is(runErr, context.Canceled) || frame.ctx.Err() != nil {
			next = "cancelled"
		}
		return rt.manager.SaveCommand(context.Background(), frame.scope, inputID, envelope.OperationID, envelope.Name, envelope.Arguments, result, next, errRef)
	})
	if commitErr != nil {
		return commitErr
	}
	return runErr
}
