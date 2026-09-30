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
		command := state.OperationCommand{Principal: rt.opts.Principal, Kind: "select_default_model", Target: "next_trace", IdempotencyKey: request.IdempotencyKey, ExpectedRevision: request.ExpectedRevision, Content: content}
		if receipt, found, err := rt.manager.FindOperation(command); found || err != nil {
			if err == nil && found {
				rt.rebindAcceptedModel(receipt, request.Model.Model)
			}
			return receipt, err
		}
		if err := validateSelectionModelOptions(request.Model.Model, len(rt.opts.ToolInfos) != 0); err != nil {
			return nil, err
		}
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
			if err == nil && found {
				rt.rebindAcceptedModel(receipt, request.Model.Model)
			}
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
		needsTools := len(rt.opts.ToolInfos) != 0
		if selectedTools := rt.findTurnSelection(view, selection.Scope, "tools"); selectedTools != nil {
			needsTools = len(selectedTools.ToolNames) != 0
		}
		if err := validateSelectionModelOptions(request.Model.Model, needsTools); err != nil {
			return nil, err
		}
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
		return rt.acceptActiveTools(ctx, request)
	})
	if err != nil {
		return state.OperationReceipt{}, err
	}
	return value.(state.OperationReceipt), nil
}

func (rt *runtime) acceptActiveTools(ctx context.Context, request SetActiveToolsRequest) (any, error) {
	if err := rt.writable(); err != nil {
		return nil, err
	}
	if request.TraceID == "" {
		return nil, invalidSelection("trace is required")
	}
	names, err := normalizeToolSelection(request.ToolNames)
	if err != nil {
		return nil, err
	}
	content, _ := json.Marshal(struct {
		TraceID      string   `json:"traceId"`
		Names        []string `json:"names"`
		InvocationID string   `json:"invocationId,omitempty"`
	}{request.TraceID, names, request.InvocationID})
	command := state.OperationCommand{Principal: rt.opts.Principal, Kind: "select_active_tools", Target: request.TraceID, IdempotencyKey: request.IdempotencyKey, ExpectedRevision: request.ExpectedRevision, Content: content}
	if receipt, found, err := rt.manager.FindOperation(command); found || err != nil {
		return receipt, err
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
	invocation := request.InvocationID
	if invocation == "" {
		invocation = trace.InvocationID
	} else if invocation != trace.InvocationID {
		return nil, stateConflictSelection("selection invocation does not belong to the trace")
	}
	names, versions, err := rt.validateToolSelection(names)
	if err != nil {
		return nil, err
	}
	selection := state.Selection{ID: agent.MustID(), Scope: agent.ExecutionScope{SessionID: rt.opts.SessionID, BranchID: view.BranchID, TraceID: request.TraceID, InvocationID: invocation, Generation: rt.generation}, Kind: "tools", State: "pending", ApplyAt: "next_turn", ToolNames: names, ToolVersions: versions}
	bound := *trace
	if chosen := rt.findTurnSelection(view, selection.Scope, "model"); chosen != nil {
		bound.ModelSelectionID = chosen.ID
	}
	if bound.ModelSelectionID != "" {
		model, err := rt.modelForTrace(&bound)
		if err != nil {
			return nil, err
		}
		if err := validateSelectionModelOptions(model, len(names) != 0); err != nil {
			return nil, err
		}
	}
	receipt, err := rt.manager.AcceptSelection(ctx, command, selection)
	if err != nil {
		return nil, err
	}
	return receipt, nil
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
			if !rt.toolSelectionAllowed(def) {
				continue
			}
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

// validateSelectionModelOptions resolves the selected model's own trusted
// configuration, never options inherited from the previous model. Raw injected
// implementations retain their existing AgenticModel contract.
func validateSelectionModelOptions(candidate einomodel.AgenticModel, needsTools bool) error {
	configured, ok := candidate.(interface{ Configuration() llm.ModelConfig })
	if !ok {
		return nil
	}
	requested := llm.RequestedOptions{RequiredCapabilities: []llm.CapabilityName{llm.CapTextStream}}
	if needsTools {
		requested.RequiredCapabilities = append(requested.RequiredCapabilities, llm.CapTools)
	}
	if bound, ok := candidate.(interface{ EffectiveOptions() llm.EffectiveOptions }); ok {
		effective := bound.EffectiveOptions()
		requested.Thinking = effective.RequestedThinking
		requested.CacheIntent = effective.RequestedCacheIntent
		requested.MaxOutputTokens = effective.MaxOutputTokens
	}
	_, err := llm.ResolveOptions(configured.Configuration(), requested)
	return err
}

// restoreDefaultSelection reconstructs only the durable reference. Opening a
// session does not construct models, activate choices, or resume execution.
func (rt *runtime) restoreDefaultSelection() {
	view := rt.manager.View()
	scope := agent.ExecutionScope{SessionID: rt.opts.SessionID, BranchID: view.BranchID, Generation: rt.generation}
	var revision uint64
	for _, selected := range view.Selections {
		if selected.Kind == "model" && selected.ApplyAt == "next_trace" && (selected.State == "pending" || selected.State == "active") && selected.Scope == scope && selected.Revision > revision {
			rt.defaultModelID, revision = selected.ID, selected.Revision
		}
	}
}

func normalizeToolSelection(names []string) ([]string, error) {
	seen := make(map[string]bool, len(names))
	ordered := append([]string(nil), names...)
	for i, name := range ordered {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return nil, invalidSelection("tool names must be unique and non-empty")
		}
		seen[name], ordered[i] = true, name
	}
	sort.Strings(ordered)
	return ordered, nil
}

func (rt *runtime) validateToolSelection(names []string) ([]string, []string, error) {
	ordered, err := normalizeToolSelection(names)
	if err != nil {
		return nil, nil, err
	}
	versions := make([]string, len(ordered))
	for i, name := range ordered {
		found := false
		for _, def := range rt.opts.Tools {
			if def.Name == name {
				if !rt.toolSelectionAllowed(def) {
					return nil, nil, product.NewError(product.CodePermissionDenied, "tool selection is unavailable or denied")
				}
				found, versions[i] = true, def.Version
				break
			}
		}
		if !found {
			return nil, nil, notFoundSelection("tool is not registered in the current generation")
		}
	}
	return ordered, versions, nil
}

// rebindAcceptedModel optionally restores a lost process-local instance through
// an exact idempotent replay. Invalid candidates leave the binding untouched;
// they cannot change the historical receipt returned by FindOperation. Resume
// still rejects a missing instance. No choice, default or revision is changed.
func (rt *runtime) rebindAcceptedModel(receipt state.OperationReceipt, candidate einomodel.AgenticModel) {
	view := rt.manager.View()
	selected, ok := selectionForOperation(view, receipt.OperationID)
	if !ok || selected.Kind != "model" || !state.SelectionMatchesOperation(selected, view.Operations[receipt.OperationID]) {
		return
	}
	if rt.modelSlots[selected.ID] != nil {
		return
	}
	if _, _, err := validateModelChoice(ModelChoice{Name: selected.ModelName, Version: selected.ModelVersion, Model: candidate}); err != nil {
		return
	}
	if err := validateSelectionModelOptions(candidate, false); err != nil {
		return
	}
	if rt.modelSlots == nil {
		rt.modelSlots = map[string]einomodel.AgenticModel{}
	}
	rt.modelSlots[selected.ID] = candidate
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
		if own, ok, err := rt.targetModel(trace); err != nil {
			return nil, err
		} else if ok {
			return own, nil
		}
		if rt.opts.Model == nil {
			return nil, product.NewError(product.CodeResourceUnavailable, "model instance is unavailable")
		}
		return rt.opts.Model, nil
	}
	selection, ok := rt.manager.View().Selections[trace.ModelSelectionID]
	if !ok || selection.Kind != "model" {
		return nil, product.NewError(product.CodeIncompatibleResume, "trace model selection is missing")
	}
	if selected := rt.modelSlots[trace.ModelSelectionID]; selected != nil {
		if _, _, err := validateModelChoice(ModelChoice{Name: selection.ModelName, Version: selection.ModelVersion, Model: selected}); err != nil {
			return nil, err
		}
		return selected, nil
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

// beginSelectedTurn runs only in the ledger's mailbox persistence callback.
// Validation and durable publication precede all execution-frame changes.
func (rt *runtime) beginSelectedTurn(ctx context.Context, frame *execution, usage agent.Usage) error {
	view := rt.manager.View()
	trace := view.Traces[frame.scope.TraceID]
	if trace == nil {
		return notFoundSelection("trace not found")
	}
	bound := *trace
	modelSelection := rt.findTurnSelection(view, frame.scope, "model")
	toolSelection := rt.findTurnSelection(view, frame.scope, "tools")
	var selectionIDs []string
	if modelSelection != nil {
		bound.ModelSelectionID = modelSelection.ID
		selectionIDs = append(selectionIDs, modelSelection.ID)
	}
	selected, err := rt.modelForTrace(&bound)
	if err != nil {
		return err
	}
	names := make(map[string]struct{})
	if toolSelection != nil {
		if _, _, err := rt.validateToolSelection(toolSelection.ToolNames); err != nil {
			return err
		}
		for _, name := range toolSelection.ToolNames {
			names[name] = struct{}{}
		}
		selectionIDs = append(selectionIDs, toolSelection.ID)
	} else {
		for _, info := range rt.opts.ToolInfos {
			if info != nil {
				names[info.Name] = struct{}{}
			}
		}
	}
	// A caller without delegation targets never sees or selects the task tool.
	if _, listed := names[delegateToolName]; listed && !rt.delegateToolAllowed(trace.ID) {
		delete(names, delegateToolName)
	}
	if bound.ModelSelectionID != "" {
		if err := validateSelectionModelOptions(selected, len(names) != 0); err != nil {
			return err
		}
	}
	turn := agent.TurnRecord{ID: usage.ModelCallID, TraceID: trace.ID, InvocationID: frame.scope.InvocationID, SelectionRevision: view.LastSeq, ModelConfigVersion: modelVersion(selected), ToolNames: sortedToolNames(names)}
	if err := rt.manager.BeginSelectedTurn(ctx, frame.scope, usage, turn, selectionIDs); err != nil {
		return err
	}
	frame.currentModel, frame.activeToolNames = selected, names
	frame.turnID, frame.turnSelectionRevision = turn.ID, turn.SelectionRevision
	frame.scope.SelectionRevision = turn.SelectionRevision
	if toolSelection != nil {
		frame.activeToolSelection = toolSelection.Revision
	}
	return nil
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
