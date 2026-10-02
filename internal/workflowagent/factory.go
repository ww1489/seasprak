package workflowagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/cloudwego/eino/components/model"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
)

type workspaceBinding struct {
	ResourceType   storage.ResourceType `json:"resourceType"`
	FormatVersion  int                  `json:"formatVersion"`
	HostRealRoot   string               `json:"hostRealRoot"`
	DefinitionHash string               `json:"definitionHash"`
	BindingVersion string               `json:"bindingVersion"`
}
type modelBinding struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Observed bool   `json:"observed,omitempty"`
}
type toolBinding struct {
	Name        string                     `json:"name"`
	Version     string                     `json:"version"`
	SchemaHash  string                     `json:"schemaHash"`
	Declaration tools.ExecutionDescription `json:"declaration"`
	Interface   string                     `json:"interface,omitempty"`
}
type manifest struct {
	ResourceType   storage.ResourceType          `json:"resourceType"`
	FormatVersion  int                           `json:"formatVersion"`
	Definition     WorkflowDefinition            `json:"definition"`
	DefinitionHash string                        `json:"definitionHash"`
	Subflows       map[string]WorkflowDefinition `json:"subflows"`
	SubflowHashes  map[string]string             `json:"subflowHashes"`
	Models         []modelBinding                `json:"models"`
	Tools          []toolBinding                 `json:"tools"`
	Fingerprint    string                        `json:"fingerprint"`
	TodoBackend    string                        `json:"todoBackend"`
}

func digest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func rawHash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func compileOptions(opts WorkflowOptions) (*CompiledWorkflow, manifest, string, error) {
	bindings := WorkflowBindings{Models: map[string]bool{}, Tools: map[string]json.RawMessage{}, Subflows: map[string]*CompiledWorkflow{}}
	saved := manifest{ResourceType: storage.ResourceWorkflow, FormatVersion: 1, Definition: opts.Definition, Subflows: opts.Subflows, SubflowHashes: map[string]string{}, Fingerprint: opts.GenerationFingerprint, TodoBackend: todoBackendIdentity(opts.Operations.Todos)}
	for name, m := range opts.Models {
		if m == nil {
			continue
		}
		bindings.Models[name] = true
		version := opts.GenerationFingerprint
		if configured, ok := m.(interface{ Configuration() llm.ModelConfig }); ok {
			version = configured.Configuration().Version
			if version == "" {
				return nil, saved, "", product.NewError(product.CodeInvalidArgument, "model binding configuration version is required")
			}
		}
		saved.Models = append(saved.Models, modelBinding{Name: name, Version: version, Observed: llm.UsesObservedTransport(m)})
	}
	for _, def := range opts.Tools {
		if def.Version == "" {
			return nil, saved, "", product.NewError(product.CodeInvalidArgument, "workflow tool implementation version is required")
		}
		if _, exists := bindings.Tools[def.Name]; exists {
			return nil, saved, "", product.NewError(product.CodeInvalidArgument, "duplicate workflow tool binding")
		}
		bindings.Tools[def.Name] = def.Schema
		saved.Tools = append(saved.Tools, toolBinding{Name: def.Name, Version: def.Version, SchemaHash: rawHash(def.Schema), Declaration: def.Execution.Clone(), Interface: def.ToolInterface})
	}
	if _, err := tools.CompileSchemas(opts.Tools); err != nil {
		return nil, saved, "", err
	}
	visiting := map[string]bool{}
	var compileSub func(string) (*CompiledWorkflow, error)
	compileSub = func(key string) (*CompiledWorkflow, error) {
		if c := bindings.Subflows[key]; c != nil {
			return c, nil
		}
		def, exists := opts.Subflows[key]
		if !exists || key != def.Name+"@"+def.Version {
			return nil, product.NewError(product.CodeInvalidArgument, "fixed subflow binding is unavailable")
		}
		if visiting[key] {
			return nil, product.NewError(product.CodeInvalidArgument, "recursive subflow dependency")
		}
		visiting[key] = true
		defer delete(visiting, key)
		for _, n := range def.Nodes {
			if n.Type == WorkflowNodeSubflow {
				if _, err := compileSub(n.Subflow); err != nil {
					return nil, err
				}
			}
		}
		c, err := CompileWorkflow(def, bindings)
		if err == nil {
			bindings.Subflows[key] = c
			saved.SubflowHashes[key] = c.Hash
		}
		return c, err
	}
	for key := range opts.Subflows {
		if _, err := compileSub(key); err != nil {
			return nil, saved, "", err
		}
	}
	c, err := CompileWorkflow(opts.Definition, bindings)
	if err != nil {
		return nil, saved, "", err
	}
	saved.Definition = c.Definition
	saved.DefinitionHash = c.Hash
	sort.Slice(saved.Models, func(i, j int) bool { return saved.Models[i].Name < saved.Models[j].Name })
	sort.Slice(saved.Tools, func(i, j int) bool { return saved.Tools[i].Name < saved.Tools[j].Name })
	return c, saved, digest(saved), nil
}

func resolveWorkspace(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", product.NewError(product.CodeInvalidArgument, "explicit absolute workspace is required")
	}
	return storage.ResolveDir(path)
}
func resolveStateRoot(root string) (string, error) {
	if root == "" {
		cfg, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(cfg, "seasprak", "state")
		if err := os.MkdirAll(root, 0700); err != nil {
			return "", err
		}
	}
	return storage.ResolveDir(root)
}

func CreateWorkflowAgent(ctx context.Context, opts WorkflowOptions) (*WorkflowAgent, error) {
	return createWorkflowAgent(ctx, opts, nil, jsonl.OpenBound)
}

func createWorkflowAgent(ctx context.Context, opts WorkflowOptions, openChild func(*os.Root, string) (*os.Root, error), finalOpenBound func(string, *storage.ResourceRoots, *os.File, storage.Header, jsonl.Options) (*jsonl.Store, error)) (out *WorkflowAgent, err error) {
	backend := opts.Store
	owned := backend == nil
	var roots *storage.ResourceRoots
	defer func() {
		if out == nil {
			if owned {
				err = closeWorkflowFactoryOwned(err, backend, nil, roots)
			} else {
				err = normalizeWorkflowFactoryError(err)
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.ReadOnly {
		return nil, product.NewError(product.CodeInvalidArgument, "creation requires a writable workflow")
	}
	ws, err := resolveWorkspace(opts.Workspace)
	if err != nil {
		return nil, err
	}
	opts.Workspace = ws
	if opts.RunID == "" {
		opts.RunID = agent.MustID()
	}
	if err := storage.ValidateResourceID(opts.RunID); err != nil {
		return nil, err
	}
	c, m, binding, err := compileOptions(opts)
	if err != nil {
		return nil, err
	}
	policy, err := normalizePolicy(opts.Policy)
	if err != nil {
		return nil, err
	}
	if err := opts.Operations.ValidateCapabilities(ctx, policy.SandboxMode); err != nil {
		return nil, err
	}
	var root string
	if opts.Store == nil {
		root, err = resolveStateRoot(opts.StateRoot)
		if err != nil {
			return nil, err
		}
		if storage.PathsOverlap(ws, root) {
			return nil, product.NewError(product.CodeInvalidArgument, "state root overlaps workspace")
		}
		opts.StateRoot = root
	}
	wb := workspaceBinding{ResourceType: storage.ResourceWorkflow, FormatVersion: 1, HostRealRoot: ws, DefinitionHash: c.Hash, BindingVersion: binding}
	raw, _ := json.Marshal(wb)
	if owned {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		roots, err = storage.OpenResourceRoots(root, storage.ResourceWorkflow, opts.RunID, true, openChild)
		if err != nil {
			return nil, err
		}
		if _, err := roots.Resource.Lstat("journal.jsonl"); err == nil {
			return nil, product.NewError(product.CodeStateConflict, "workflow run already exists")
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		opened, err := finalOpenBound(opts.RunID, roots, nil, storage.Header{ResourceType: storage.ResourceWorkflow, RunID: opts.RunID, Workspace: raw}, jsonl.Options{ResourceType: storage.ResourceWorkflow, MaxLine: opts.Limits.WithDefaults().MaxCommitLineBytes})
		if err != nil {
			return nil, err
		}
		backend = opened
		roots = nil // Successful OpenBound transferred ownership to the backend.
	}
	loaded, err := backend.Load(ctx, opts.RunID)
	if err == nil {
		err = storage.ValidateHeader(loaded.Header, storage.ResourceWorkflow, opts.RunID)
	}
	if err == nil && len(loaded.Commits) != 0 {
		err = product.NewError(product.CodeStateConflict, "workflow run already initialized")
	}
	if err == nil {
		err = validateBinding(loaded.Header.Workspace, wb)
	}
	if err != nil {
		return nil, err
	}
	w := assemble(opts, backend, c, binding)
	init := initialRecord{RunID: opts.RunID, Workspace: ws, Manifest: m, BindingVersion: binding, Principal: opts.Principal, Policy: policy, Limits: opts.Limits.WithDefaults()}
	w.mu.Lock()
	err = w.commitLocked(ctx, []storage.Record{record("workflow_initialized", "initial", init)}, []agent.Event{w.event("workflow.created", "", map[string]any{"state": "created"})})
	w.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return w, nil
}

func OpenWorkflowAgent(ctx context.Context, opts WorkflowOptions) (*WorkflowAgent, error) {
	return openWorkflowAgent(ctx, opts, nil, jsonl.OpenBound)
}

func openWorkflowAgent(ctx context.Context, opts WorkflowOptions, openChild func(*os.Root, string) (*os.Root, error), finalOpenBound func(string, *storage.ResourceRoots, *os.File, storage.Header, jsonl.Options) (*jsonl.Store, error)) (out *WorkflowAgent, err error) {
	backend := opts.Store
	owned := backend == nil
	var roots *storage.ResourceRoots
	var inspected *os.File
	defer func() {
		if out == nil {
			if owned {
				err = closeWorkflowFactoryOwned(err, backend, inspected, roots)
			} else {
				err = normalizeWorkflowFactoryError(err)
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := storage.ValidateResourceID(opts.RunID); err != nil {
		return nil, err
	}
	var loaded storage.StoredSession
	if owned {
		opts.StateRoot, err = resolveStateRootForOpen(opts.StateRoot)
		if err != nil {
			return nil, err
		}
		roots, err = storage.OpenResourceRoots(opts.StateRoot, storage.ResourceWorkflow, opts.RunID, false, openChild)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, product.NewError(product.CodeNotFound, "workflow run does not exist")
			}
			return nil, err
		}
		inspected, err = jsonl.OpenJournalReader(roots.Resource)
		if err != nil {
			return nil, err
		}
		info, statErr := inspected.Stat()
		if statErr != nil {
			return nil, statErr
		}
		loaded, err = jsonl.ReadFile(io.NewSectionReader(inspected, 0, info.Size()), opts.RunID, opts.Limits.WithDefaults().MaxCommitLineBytes)
	} else {
		loaded, err = backend.Load(ctx, opts.RunID)
	}
	if err != nil {
		return nil, err
	}
	if err := storage.ValidateHeader(loaded.Header, storage.ResourceWorkflow, opts.RunID); err != nil {
		return nil, err
	}
	var wb workspaceBinding
	if json.Unmarshal(loaded.Header.Workspace, &wb) != nil || wb.ResourceType != storage.ResourceWorkflow || wb.FormatVersion != 1 || wb.HostRealRoot == "" {
		return nil, product.NewError(product.CodeIncompatibleVersion, "workflow workspace binding is incompatible")
	}
	if opts.Workspace != "" {
		provided, e := resolveWorkspace(opts.Workspace)
		if e != nil || !storage.SamePath(provided, wb.HostRealRoot) {
			return nil, product.NewError(product.CodeStateConflict, "workspace binding does not match")
		}
	}
	opts.Workspace = wb.HostRealRoot
	if !opts.ReadOnly {
		if _, err := resolveWorkspace(opts.Workspace); err != nil {
			return nil, err
		}
		if opts.StateRoot != "" && storage.PathsOverlap(opts.Workspace, opts.StateRoot) {
			return nil, product.NewError(product.CodeInvalidArgument, "state root overlaps workspace")
		}
	}
	replayed, err := replay(loaded, opts.RunID)
	if err != nil {
		return nil, err
	}
	if opts.Principal != replayed.Initial.Principal {
		return nil, product.NewError(product.CodePermissionDenied, "principal does not own this workflow run")
	}
	if replayed.Initial.BindingVersion != wb.BindingVersion || replayed.Initial.Manifest.DefinitionHash != wb.DefinitionHash || !storage.SamePath(replayed.Initial.Workspace, wb.HostRealRoot) {
		return nil, product.NewError(product.CodeIncompatibleVersion, "workflow binding does not match committed manifest")
	}
	var c *CompiledWorkflow
	if !opts.ReadOnly {
		var m manifest
		var binding string
		c, m, binding, err = compileOptions(opts)
		_ = m
		if err != nil {
			return nil, product.NewError(product.CodeIncompatibleVersion, "persisted workflow bindings cannot be reconstructed")
		}
		if binding != replayed.Initial.BindingVersion {
			return nil, product.NewError(product.CodeIncompatibleVersion, "workflow definition or binding version differs")
		}
		policy, err := normalizePolicy(opts.Policy)
		if err != nil {
			return nil, err
		}
		if opts.Policy != nil && (policy.SandboxMode != replayed.Initial.Policy.SandboxMode || policy.ApprovalPolicy != replayed.Initial.Policy.ApprovalPolicy) {
			return nil, product.NewError(product.CodeStateConflict, "workflow execution policy differs")
		}
		if err := opts.Operations.ValidateCapabilities(ctx, replayed.Initial.Policy.SandboxMode); err != nil {
			return nil, err
		}
	}
	if owned {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		opened, err := finalOpenBound(opts.RunID, roots, inspected, storage.Header{}, jsonl.Options{ResourceType: storage.ResourceWorkflow, OpenExisting: true, ReadOnly: opts.ReadOnly, MaxLine: opts.Limits.WithDefaults().MaxCommitLineBytes})
		if err != nil {
			return nil, err
		}
		backend = opened
		roots = nil // The inspection file stays factory-owned after transfer.
		closeErr := inspected.Close()
		inspected = nil
		if closeErr != nil {
			return nil, closeErr
		}
		latest, err := backend.Load(ctx, opts.RunID)
		if err != nil {
			return nil, err
		}
		replayed, err = replayWorkflowFactoryLatest(latest, loaded, replayed, opts.RunID)
		if err != nil {
			return nil, err
		}
	}
	w := assemble(opts, backend, c, wb.BindingVersion)
	w.state = replayed
	if !opts.ReadOnly {
		if err := w.restoreHolds(); err != nil {
			return nil, err
		}
	}
	return w, nil
}
func resolveStateRootForOpen(root string) (string, error) {
	if root == "" {
		return "", product.NewError(product.CodeInvalidArgument, "state root is required for open")
	}
	return storage.ResolveDir(root)
}
func validateBinding(raw json.RawMessage, want workspaceBinding) error {
	var got workspaceBinding
	if json.Unmarshal(raw, &got) != nil || got != want {
		return product.NewError(product.CodeIncompatibleVersion, "injected workflow store binding is incompatible")
	}
	return nil
}
func assemble(opts WorkflowOptions, backend storage.Store, c *CompiledWorkflow, binding string) *WorkflowAgent {
	opts.Limits = opts.Limits.WithDefaults()
	defs := make([]tools.Definition, len(opts.Tools))
	for i := range opts.Tools {
		defs[i] = opts.Tools[i].Clone()
	}
	opts.Tools = defs
	models := make(map[string]model.AgenticModel, len(opts.Models))
	for name, m := range opts.Models {
		models[name] = m
	}
	opts.Models = models
	w := &WorkflowAgent{opts: opts, store: backend, state: newRunState(), compiled: c, binding: binding, instance: agent.MustID(), approvals: map[string]*runtimeApproval{}, approvalOps: map[string]WorkflowOperation{}, subs: map[*WorkflowSubscription]struct{}{}, transient: WorkflowTransient{Models: map[string]agent.ModelStreamSnapshot{}, Tools: map[string]WorkflowToolPreview{}}, streamSeq: map[string]uint64{}, callStreams: map[string]string{}, scheduler: tools.SharedResourceScheduler(), closeDone: make(chan struct{})}
	if w.opts.Operations.Todos == nil {
		w.opts.Operations.Todos = &workflowTodos{owner: w}
	}
	return w
}
