package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func toolArgumentHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// saveFrozenExecution runs in the session mailbox, which owns the accepted
// model call and the current policy. It never rewrites FrozenCall's original
// arguments or grants authority merely because a descriptor was recorded.
func (rt *runtime) saveFrozenExecution(ctx context.Context, scope agent.ExecutionScope, frozen agent.FrozenExecution) error {
	v := rt.manager.View()
	call, ok := v.Calls[frozen.CallID]
	if !ok || call.Claimed || call.Observation != nil || !acceptedAttemptForCall(v, call) ||
		!rt.matchesCallScope(scope, call) || frozen.Scope != call.Scope || frozen.ID != "execution:"+call.Call.CallID ||
		frozen.Origin != "model" || frozen.ProviderCallID != call.Call.ProviderCallID ||
		frozen.Tool != call.Call.Name || frozen.Generation != call.Call.Generation ||
		frozen.OriginalArgumentsHash != toolArgumentHash([]byte(call.Call.Arguments)) ||
		frozen.FinalArgumentsHash != toolArgumentHash(frozen.FinalArguments) || frozen.PolicyRef != v.ExecutionPolicy.Ref {
		return product.NewError(product.CodePermissionDenied, "frozen execution does not match the accepted call or policy")
	}
	if digest, err := frozen.Digest(); err != nil || frozen.Hash == "" || frozen.Hash != digest {
		return product.NewError(product.CodePermissionDenied, "frozen execution hash is invalid")
	}
	dec := json.NewDecoder(bytes.NewReader(frozen.FinalArguments))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return product.NewError(product.CodeInvalidArgument, "final execution arguments are invalid")
	}
	if err := dec.Decode(&value); err != io.EOF {
		return product.NewError(product.CodeInvalidArgument, "final execution arguments have trailing content")
	}
	registered := false
	for _, def := range rt.opts.Tools {
		if def.Name == frozen.Tool && def.Version == frozen.ToolVersion && toolArgumentHash(def.Schema) == frozen.SchemaHash {
			registered = true
			break
		}
	}
	if !registered {
		return product.NewError(product.CodePermissionDenied, "tool implementation is unavailable")
	}
	return rt.manager.SaveRecords(ctx, v.LastSeq, state.Records{FrozenExecutions: []state.FrozenExecution{frozen.Clone()}})
}

func (rt *runtime) ValidateExecutionTicket(ctx context.Context, frozen agent.FrozenExecution) error {
	return rt.do(ctx, func(rt *runtime) error {
		decision, err := rt.checkToolPolicyState(ctx, frozen.Scope, frozen, true)
		if err != nil {
			return err
		}
		if decision != agent.DecisionAllow {
			return product.NewError(product.CodePermissionDenied, "execution policy no longer allows the tool")
		}
		return nil
	})
}

func (rt *runtime) resourceScheduler() *tools.ResourceScheduler {
	if rt.opts.ResourceScheduler != nil {
		return rt.opts.ResourceScheduler
	}
	return tools.SharedResourceScheduler()
}
func (rt *runtime) resourceDomain() (string, string) {
	environment := rt.opts.ResourceEnvironment
	if environment == "" {
		environment = "memory"
	}
	workspace := rt.opts.Workspace
	if workspace == "" {
		workspace = rt.opts.SessionID
	}
	return environment, workspace
}

// Historical claimed calls without known terminal effects retain their whole
// scheduling domain on reopen. A missing descriptor takes the workspace guard.
func (rt *runtime) restoreResourceHolds() error {
	v := rt.manager.View()
	environment, workspace := rt.resourceDomain()
	scheduler := rt.resourceScheduler()
	var restore []tools.ResourceHold
	var release []string
	for id, call := range v.Calls {
		if !call.Claimed {
			continue
		}
		holdID := tools.ResourceHoldID(rt.opts.SessionID, id)
		if call.Observation != nil && call.Observation.SideEffect != "unknown" && call.Observation.Status != "outcome_unknown" {
			release = append(release, holdID)
			continue
		}
		request := tools.ResourceRequest{Environment: environment, Workspace: workspace, Effect: "unknown"}
		if frozen, ok := v.FrozenExecutions["execution:"+id]; ok {
			if hash, err := frozen.Digest(); err == nil && frozen.Hash == hash && frozen.Hash != "" {
				request.Resources, request.Effect, request.Concurrency = frozen.Resources, frozen.Effect, frozen.Concurrency
			}
		}
		restore = append(restore, tools.ResourceHold{ID: holdID, Request: request})
	}
	if err := scheduler.RestoreHolds(restore); err != nil {
		return err
	}
	for _, id := range release {
		if err := scheduler.ReleaseHold(id); err != nil {
			return err
		}
	}
	return nil
}
