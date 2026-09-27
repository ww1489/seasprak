package sessions

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

type ModelChoice struct {
	Name    string                 `json:"name"`
	Version string                 `json:"version"`
	Model   einomodel.AgenticModel `json:"-"`
}

type SetDefaultModelRequest struct {
	Model            ModelChoice `json:"model"`
	ExpectedRevision uint64      `json:"expectedRevision"`
	IdempotencyKey   string      `json:"idempotencyKey,omitempty"`
}

type SelectNextTurnModelRequest struct {
	TraceID          string      `json:"traceId"`
	Model            ModelChoice `json:"model"`
	ExpectedRevision uint64      `json:"expectedRevision"`
	IdempotencyKey   string      `json:"idempotencyKey,omitempty"`
}

type SetActiveToolsRequest struct {
	TraceID          string   `json:"traceId"`
	InvocationID     string   `json:"invocationId,omitempty"`
	ToolNames        []string `json:"toolNames"`
	ExpectedRevision uint64   `json:"expectedRevision"`
	IdempotencyKey   string   `json:"idempotencyKey,omitempty"`
}

type SearchToolsRequest struct {
	TraceID string `json:"traceId,omitempty"`
	Query   string `json:"query,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type ToolCandidate struct {
	Name        string           `json:"name"`
	Version     string           `json:"version"`
	Description string           `json:"description,omitempty"`
	Generation  string           `json:"generation"`
	Info        *schema.ToolInfo `json:"info,omitempty"`
}

func (s *AgentSession) SetDefaultModel(ctx context.Context, request SetDefaultModelRequest) (state.OperationReceipt, error) {
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		name, version, err := validateModelChoice(request.Model)
		if err != nil {
			return nil, err
		}
		view := rt.manager.View()
		selection := state.Selection{ID: agent.MustID(), Scope: agent.ExecutionScope{SessionID: rt.opts.SessionID, BranchID: view.BranchID, Generation: rt.generation}, Kind: "model", State: "pending", ApplyAt: "next_trace", ModelName: name, ModelVersion: version}
		content, _ := json.Marshal(struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}{name, version})
		receipt, err := rt.manager.AcceptSelection(ctx, state.OperationCommand{Principal: rt.opts.Principal, Kind: "select_default_model", Target: "next_trace", IdempotencyKey: request.IdempotencyKey, ExpectedRevision: request.ExpectedRevision, Content: content}, selection)
		if err != nil {
			return nil, err
		}
		if rt.modelSlots == nil {
			rt.modelSlots = map[string]einomodel.AgenticModel{}
		}
		// A replayed receipt refers to the original selection, not this request's
		// new ID. Replays must not replace its instance or reset the default.
		if selected, ok := selectionForOperation(rt.manager.View(), receipt.OperationID); ok && selected.ID == selection.ID {
			rt.modelSlots[selected.ID] = request.Model.Model
			rt.defaultModelID = selected.ID
		}
		return receipt, nil
	})
	if err != nil {
		return state.OperationReceipt{}, err
	}
	return value.(state.OperationReceipt), nil
}

func (s *AgentSession) SelectNextTurnModel(ctx context.Context, request SelectNextTurnModelRequest) (state.OperationReceipt, error) {
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		if request.TraceID == "" {
			return nil, invalidSelection("trace is required")
		}
		name, version, err := validateModelChoice(request.Model)
		if err != nil {
			return nil, err
		}
		content, _ := json.Marshal(struct {
			TraceID string `json:"traceId"`
			Name    string `json:"name"`
			Version string `json:"version"`
		}{request.TraceID, name, version})
		command := state.OperationCommand{Principal: rt.opts.Principal, Kind: "select_next_turn_model", Target: request.TraceID, IdempotencyKey: request.IdempotencyKey, ExpectedRevision: request.ExpectedRevision, Content: content}
		if receipt, found, err := rt.manager.FindOperation(command); found || err != nil {
			return receipt, err
		}
		view := rt.manager.View()
		trace := view.Traces[request.TraceID]
		if trace == nil {
			return nil, notFoundSelection("trace not found")
		}
		if terminal(trace.State) || trace.State == "cancelling" {
			return nil, stateConflictSelection("trace cannot accept a next-turn model")
		}
		if trace.Generation != rt.generation {
			return nil, incompatibleSelection("model generation does not match the trace")
		}
		invocation := ""
		if trace.InvocationID != "" {
			invocation = trace.InvocationID
		}
		selection := state.Selection{ID: agent.MustID(), Scope: agent.ExecutionScope{SessionID: rt.opts.SessionID, BranchID: view.BranchID, TraceID: request.TraceID, InvocationID: invocation, Generation: rt.generation}, Kind: "model", State: "pending", ApplyAt: "next_turn", ModelName: name, ModelVersion: version}
		receipt, err := rt.manager.AcceptSelection(ctx, command, selection)
		if err != nil {
			return nil, err
		}
		if rt.modelSlots == nil {
			rt.modelSlots = map[string]einomodel.AgenticModel{}
		}
		// A replayed receipt refers to the original selection, not this request's
		// new ID. Replays must not replace its instance or reset the default.
		if selected, ok := selectionForOperation(rt.manager.View(), receipt.OperationID); ok && selected.ID == selection.ID {
			rt.modelSlots[selected.ID] = request.Model.Model
		}
		return receipt, nil
	})
	if err != nil {
		return state.OperationReceipt{}, err
	}
	return value.(state.OperationReceipt), nil
}

func (s *AgentSession) SetActiveTools(ctx context.Context, request SetActiveToolsRequest) (state.OperationReceipt, error) {
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		if request.TraceID == "" {
			return nil, invalidSelection("trace is required")
		}
		view := rt.manager.View()
		trace := view.Traces[request.TraceID]
		if trace == nil {
			return nil, notFoundSelection("trace not found")
		}
		if terminal(trace.State) || trace.State == "cancelling" {
			return nil, stateConflictSelection("trace cannot accept a tool selection")
		}
		if trace.Generation != rt.generation {
			return nil, incompatibleSelection("tool generation does not match the trace")
		}
		names, versions, err := rt.validateToolSelection(request.ToolNames)
		if err != nil {
			return nil, err
		}
		invocation := request.InvocationID
		if invocation == "" {
			invocation = trace.InvocationID
		}
		selection := state.Selection{ID: agent.MustID(), Scope: agent.ExecutionScope{SessionID: rt.opts.SessionID, BranchID: view.BranchID, TraceID: request.TraceID, InvocationID: invocation, Generation: rt.generation}, Kind: "tools", State: "pending", ApplyAt: "next_turn", ToolNames: names, ToolVersions: versions}
		content, _ := json.Marshal(struct {
			TraceID string   `json:"traceId"`
			Names   []string `json:"names"`
		}{request.TraceID, names})
		receipt, err := rt.manager.AcceptSelection(ctx, state.OperationCommand{Principal: rt.opts.Principal, Kind: "select_active_tools", Target: request.TraceID, IdempotencyKey: request.IdempotencyKey, ExpectedRevision: request.ExpectedRevision, Content: content}, selection)
		if err != nil {
			return nil, err
		}
		return receipt, nil
	})
	if err != nil {
		return state.OperationReceipt{}, err
	}
	return value.(state.OperationReceipt), nil
}

func (s *AgentSession) SearchTools(ctx context.Context, request SearchToolsRequest) ([]ToolCandidate, error) {
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if request.TraceID != "" {
			trace := rt.manager.View().Traces[request.TraceID]
			if trace == nil {
				return nil, notFoundSelection("trace not found")
			}
			if trace.Generation != rt.generation {
				return nil, incompatibleSelection("tool generation does not match the trace")
			}
		}
		limit := request.Limit
		if limit == 0 {
			limit = 50
		}
		if limit < 0 || limit > 100 {
			return nil, invalidSelection("tool search limit is out of range")
		}
		query := strings.ToLower(strings.TrimSpace(request.Query))
		result := make([]ToolCandidate, 0, limit)
		for _, def := range rt.opts.Tools {
			if query != "" && !strings.Contains(strings.ToLower(def.Name), query) && !strings.Contains(strings.ToLower(def.Description), query) {
				continue
			}
			result = append(result, ToolCandidate{Name: def.Name, Version: def.Version, Description: def.Description, Generation: rt.generation, Info: toolInfoFor(rt.opts.ToolInfos, def.Name)})
		}
		sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
		if len(result) > limit {
			result = result[:limit]
		}
		return result, nil
	})
	if err != nil {
		return nil, err
	}
	return value.([]ToolCandidate), nil
}

func validateModelChoice(choice ModelChoice) (string, string, error) {
	name, version := strings.TrimSpace(choice.Name), strings.TrimSpace(choice.Version)
	if configured, ok := choice.Model.(interface{ Configuration() llm.ModelConfig }); ok {
		cfg := configured.Configuration()
		if name == "" {
			name = cfg.Model
		}
		if version == "" {
			version = cfg.Version
		}
		if (choice.Name != "" && choice.Name != cfg.Model) || (choice.Version != "" && choice.Version != cfg.Version) {
			return "", "", stateConflictSelection("model choice does not match its configuration")
		}
	}
	if choice.Model == nil || name == "" || version == "" {
		return "", "", invalidSelection("model name, version and instance are required")
	}
	return name, version, nil
}

func (rt *runtime) validateToolSelection(names []string) ([]string, []string, error) {
	seen := make(map[string]bool, len(names))
	versions := make(map[string]string, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return nil, nil, invalidSelection("tool names must be unique and non-empty")
		}
		found := false
		for _, def := range rt.opts.Tools {
			if def.Name == name {
				found = true
				versions[name] = def.Version
				break
			}
		}
		if !found {
			return nil, nil, notFoundSelection("tool is not registered in the current generation")
		}
		seen[name] = true
	}
	ordered := append([]string(nil), names...)
	sort.Strings(ordered)
	orderedVersions := make([]string, len(ordered))
	for i, name := range ordered {
		orderedVersions[i] = versions[name]
	}
	return ordered, orderedVersions, nil
}

func selectionForOperation(view state.View, operationID string) (state.Selection, bool) {
	for _, selection := range view.Selections {
		if selection.OperationID == operationID {
			return selection, true
		}
	}
	return state.Selection{}, false
}

func toolInfoFor(infos []*schema.ToolInfo, name string) *schema.ToolInfo {
	for _, info := range infos {
		if info != nil && info.Name == name {
			return info
		}
	}
	return nil
}

func invalidSelection(message string) error {
	return product.NewError(product.CodeInvalidArgument, message)
}
func notFoundSelection(message string) error { return product.NewError(product.CodeNotFound, message) }
func stateConflictSelection(message string) error {
	return product.NewError(product.CodeStateConflict, message)
}
func incompatibleSelection(message string) error {
	return product.NewError(product.CodeIncompatibleVersion, message)
}

func (rt *runtime) modelForTrace(trace *state.TraceState) (einomodel.AgenticModel, error) {
	if trace == nil {
		return nil, notFoundSelection("trace not found")
	}
	if trace.ModelSelectionID == "" {
		if rt.opts.Model == nil {
			return nil, product.NewError(product.CodeResourceUnavailable, "model instance is unavailable")
		}
		return rt.opts.Model, nil
	}
	if rt.modelSlots != nil {
		if selected := rt.modelSlots[trace.ModelSelectionID]; selected != nil {
			return selected, nil
		}
	}
	selection, ok := rt.manager.View().Selections[trace.ModelSelectionID]
	if !ok {
		return nil, product.NewError(product.CodeIncompatibleResume, "trace model selection is missing")
	}
	if rt.opts.Model != nil {
		name, version, err := modelIdentity(rt.opts.Model)
		if err == nil && name == selection.ModelName && version == selection.ModelVersion {
			return rt.opts.Model, nil
		}
	}
	return nil, product.NewError(product.CodeResourceUnavailable, "selected model instance is unavailable")
}

// modelForCheckpoint resolves only choices committed for the checkpoint's
// original Turn. Pending choices and the current session default are not resume
// identities, even when they name a compatible model version.
func (rt *runtime) modelForCheckpoint(cp state.CheckpointRef, view state.View) (einomodel.AgenticModel, error) {
	trace := view.Traces[cp.Scope.TraceID]
	turn, ok := view.Turns[cp.Scope.TurnID]
	if trace == nil || !ok || turn.SelectionRevision != cp.SelectionRevision || turn.ModelConfigVersion != cp.ModelConfigVersion {
		return nil, incompatibleResume("checkpoint turn model snapshot differs")
	}
	bound := *trace
	var revision uint64
	for _, selected := range view.Selections {
		if selected.Kind != "model" || selected.State != "active" || selected.ApplyAt != "next_turn" || selected.Revision > cp.SelectionRevision || selected.Revision <= revision {
			continue
		}
		scope := selected.Scope
		if scope.SessionID != cp.Scope.SessionID || scope.BranchID != cp.Scope.BranchID || scope.TraceID != cp.Scope.TraceID || scope.Generation != cp.Scope.Generation || (scope.InvocationID != "" && scope.InvocationID != cp.Scope.InvocationID) {
			continue
		}
		bound.ModelSelectionID, revision = selected.ID, selected.Revision
	}
	return rt.modelForTrace(&bound)
}

func modelIdentity(candidate einomodel.AgenticModel) (string, string, error) {
	configured, ok := candidate.(interface{ Configuration() llm.ModelConfig })
	if !ok {
		return "", "", invalidSelection("model configuration identity is unavailable")
	}
	cfg := configured.Configuration()
	if strings.TrimSpace(cfg.Model) == "" || strings.TrimSpace(cfg.Version) == "" {
		return "", "", invalidSelection("model configuration identity is incomplete")
	}
	return cfg.Model, cfg.Version, nil
}

type turnModel struct{ frame *execution }

func (m *turnModel) ResolveModel(_ context.Context, _ agent.ExecutionScope) (einomodel.AgenticModel, error) {
	if m == nil || m.frame == nil || m.frame.currentModel == nil {
		return nil, product.NewError(product.CodeResourceUnavailable, "model instance is unavailable")
	}
	return m.frame.currentModel, nil
}
func (m *turnModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.AgenticMessage, error) {
	inner, err := m.ResolveModel(ctx, agent.ExecutionScope{})
	if err != nil {
		return nil, err
	}
	return inner.Generate(ctx, input, opts...)
}
func (m *turnModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	inner, err := m.ResolveModel(ctx, agent.ExecutionScope{})
	if err != nil {
		return nil, err
	}
	return inner.Stream(ctx, input, opts...)
}
func (m *turnModel) Configuration() llm.ModelConfig {
	if m != nil && m.frame != nil && m.frame.currentModel != nil {
		if configured, ok := m.frame.currentModel.(interface{ Configuration() llm.ModelConfig }); ok {
			return configured.Configuration()
		}
	}
	return llm.ModelConfig{}
}
func (m *turnModel) UsesObservedTransport() bool {
	if m == nil || m.frame == nil {
		return false
	}
	return llm.UsesObservedTransport(m.frame.currentModel)
}
