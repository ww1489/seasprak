package codeagent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
)

// Bind only the SDK builtin; application-defined tools with the same name keep
// their implementation. The definition and schema stay generation-bound.
func (rt *runtime) searchToolDefinitions() []tools.Definition {
	defs := append([]tools.Definition(nil), rt.opts.Tools...)
	for i := range defs {
		if defs[i].Name == "search_tools" && defs[i].Version == "search-tools-v1" && defs[i].Execution.BackendID == "trusted-run" {
			defs[i].Run = rt.runToolSearch
		}
		if isDelegateBuiltin(defs[i]) {
			defs[i].Run = rt.runDelegateTask
		}
	}
	return defs
}

func (rt *runtime) toolSelectionAllowed(def tools.Definition) bool {
	policy := rt.manager.View().ExecutionPolicy
	if policy.SandboxMode == "read-only" && def.Execution.Effect != "read" && def.Execution.Effect != "none" {
		return false
	}
	if policy.ApprovalPolicy == "never" && def.Execution.RequestedGrantRef != "" {
		return false
	}
	switch def.Execution.BackendID {
	case "", "trusted-run":
		return def.Run != nil || def.RunWithOutput != nil
	case "file-operations":
		return rt.opts.Operations.Files != nil
	case "process-operations":
		return rt.opts.Operations.Process != nil
	case "todo-operations":
		return rt.opts.Operations.Todos != nil
	case "artifact-store":
		return rt.opts.Operations.Artifacts != nil
	}
	return false
}

func (rt *runtime) runToolSearch(ctx context.Context, raw json.RawMessage) (string, error) {
	scope := einorun.ScopeFromContext(ctx, agent.ExecutionScope{})
	providerID := compose.GetToolCallID(ctx)
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		if !rt.matchesExecution(scope) || scope.Generation != rt.generation {
			return nil, stateConflictSelection("tool search execution is no longer active")
		}
		view := rt.manager.View()
		var callID string
		for _, call := range view.Calls {
			if call.Scope == scope && call.Call.ProviderCallID == providerID && call.Call.Name == "search_tools" && call.Claimed && call.Observation == nil {
				callID = call.Call.CallID
				break
			}
		}
		if callID == "" {
			return nil, product.NewError(product.CodePermissionDenied, "tool search requires its claimed model call")
		}
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(raw, &args); err != nil || strings.TrimSpace(args.Query) == "" {
			return nil, invalidSelection("invalid search arguments")
		}
		infos := make([]*schema.ToolInfo, 0, len(rt.opts.ToolInfos))
		allowed := map[string]bool{}
		for _, def := range rt.opts.Tools {
			if def.Name != "search_tools" && rt.toolSelectionAllowed(def) {
				if info := toolInfoFor(rt.opts.ToolInfos, def.Name); info != nil {
					infos = append(infos, info)
					allowed[def.Name] = true
				}
			}
		}
		// Eino ignores unknown exact names. Product requests must instead be atomic.
		if query := strings.TrimSpace(args.Query); strings.HasPrefix(query, "select:") {
			for _, name := range strings.Split(strings.TrimPrefix(query, "select:"), ",") {
				name = strings.TrimSpace(name)
				if toolInfoFor(rt.opts.ToolInfos, name) == nil {
					return nil, notFoundSelection("requested tool is not registered")
				}
				if !allowed[name] {
					return nil, product.NewError(product.CodePermissionDenied, "requested tool is unavailable or denied")
				}
			}
		}
		var matches struct {
			Matches []string `json:"matches"`
		}
		if len(infos) > 0 {
			matcher, err := einorun.NewToolSearchMatcher(ctx, infos)
			if err != nil {
				return nil, err
			}
			result, err := matcher.InvokableRun(ctx, string(raw))
			if err != nil {
				return nil, invalidSelection("invalid tool search query")
			}
			if err := json.Unmarshal([]byte(result), &matches); err != nil {
				return nil, err
			}
		}
		candidates := make([]ToolCandidate, 0, len(matches.Matches))
		names := map[string]bool{}
		turn, ok := view.Turns[scope.TurnID]
		if !ok {
			return nil, stateConflictSelection("search turn is missing")
		}
		for _, name := range turn.ToolNames {
			names[name] = true
		}
		// Merge with the newest accepted pending selection inside this same mailbox.
		if selected := rt.findTurnSelection(view, scope, "tools"); selected != nil {
			names = map[string]bool{}
			for _, name := range selected.ToolNames {
				names[name] = true
			}
		}
		for _, name := range matches.Matches {
			names[name] = true
			for _, def := range rt.opts.Tools {
				if def.Name == name {
					candidates = append(candidates, ToolCandidate{Name: name, Version: def.Version, Description: def.Description, Generation: rt.generation, Info: toolInfoFor(infos, name)})
					break
				}
			}
		}
		if len(candidates) > 0 {
			selected := make([]string, 0, len(names))
			for name := range names {
				selected = append(selected, name)
			}
			if _, err := rt.acceptActiveTools(ctx, SetActiveToolsRequest{TraceID: scope.TraceID, InvocationID: scope.InvocationID, ToolNames: selected, ExpectedRevision: view.LastSeq, IdempotencyKey: "search_tools:" + callID}); err != nil {
				return nil, err
			}
		}
		return json.Marshal(struct {
			Candidates []ToolCandidate `json:"candidates"`
			ApplyAt    string          `json:"applyAt"`
		}{candidates, "next_turn"})
	})
	if err != nil {
		// A rejected query has no selection side effect. Return a structured
		// search result rather than an ambiguous trusted-run execution failure.
		if pe, ok := product.AsError(err); ok && (pe.Code == product.CodePermissionDenied || pe.Code == product.CodeInvalidArgument || pe.Code == product.CodeNotFound) {
			result, _ := json.Marshal(struct {
				Status     string          `json:"status"`
				Code       string          `json:"code"`
				Candidates []ToolCandidate `json:"candidates"`
			}{"denied", pe.Code, []ToolCandidate{}})
			return string(result), nil
		}
		return "", err
	}
	return string(value.([]byte)), nil
}
